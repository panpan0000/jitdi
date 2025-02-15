package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/v1/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"

	"github.com/wzshiming/jitdi/pkg/apis/v1alpha1"
	"github.com/wzshiming/jitdi/pkg/atomic"
	"github.com/wzshiming/jitdi/pkg/client/clientset/versioned"
	"github.com/wzshiming/jitdi/pkg/pattern"
)

type Handler struct {
	buildMutex atomic.SyncMap[string, *sync.RWMutex]
	image      *imageBuilder

	rules []*pattern.Rule

	crMut     sync.Mutex
	cr        []*pattern.Rule
	store     cache.Store
	clientset *versioned.Clientset
}

func NewHandler(cache string, config []*v1alpha1.ImageSpec, clientset *versioned.Clientset) (*Handler, error) {
	rules := make([]*pattern.Rule, 0, len(config))
	for _, c := range config {
		r, err := pattern.NewRule(c)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	builder, err := newImageBuilder(cache)
	if err != nil {
		return nil, err
	}

	sort.Slice(rules, func(i, j int) bool {
		return rules[i].LessThan(rules[j])
	})
	h := &Handler{
		image:     builder,
		rules:     rules,
		clientset: clientset,
	}

	if clientset != nil {
		go h.start(context.Background())
	}

	return h, nil
}

func (h *Handler) start(ctx context.Context) {
	api := h.clientset.ApisV1alpha1().Images()
	store, controller := cache.NewInformer(
		&cache.ListWatch{
			ListFunc: func(opts metav1.ListOptions) (runtime.Object, error) {
				return api.List(ctx, opts)
			},
			WatchFunc: func(opts metav1.ListOptions) (watch.Interface, error) {
				return api.Watch(ctx, opts)
			},
		},
		&v1alpha1.Image{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				h.resetCR()
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				h.resetCR()
			},
			DeleteFunc: func(obj interface{}) {
				h.resetCR()
			},
		},
	)
	h.store = store
	controller.Run(ctx.Done())
}

func (h *Handler) resetCR() {
	h.crMut.Lock()
	defer h.crMut.Unlock()
	h.cr = nil
}

func (h *Handler) getRules() []*pattern.Rule {
	if h.store == nil {
		return h.rules
	}

	h.crMut.Lock()
	defer h.crMut.Unlock()
	if h.cr == nil {
		list := h.store.List()
		cr := make([]*pattern.Rule, 0, len(h.rules)+len(list))
		cr = append(cr, h.rules...)

		for _, item := range list {
			image := item.(*v1alpha1.Image)
			r, err := pattern.NewRule(&image.Spec)
			if err != nil {
				slog.Error("newImageRule", "err", err)
				continue
			}
			cr = append(cr, r)
		}
		sort.Slice(cr, func(i, j int) bool {
			return cr[i].LessThan(cr[j])
		})

		h.cr = cr
	}

	return h.cr
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !strings.HasPrefix(r.URL.Path, "/v2/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if r.URL.Path == "/v2/" {
		w.Write([]byte("ok"))
		return
	}

	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	image := strings.Join(parts[2:len(parts)-2], "/")

	typ := parts[len(parts)-2]
	switch typ {
	case "blobs":
		h.blobs(w, r, image, parts[len(parts)-1])
	case "manifests":
		h.manifests(w, r, image, parts[len(parts)-1])
	}
}

func (h *Handler) blobs(w http.ResponseWriter, r *http.Request, image, hash string) {
	http.ServeFile(w, r, h.image.BlobsPath(hash))
}

func (h *Handler) manifests(w http.ResponseWriter, r *http.Request, image, tag string) {
	if strings.HasPrefix(tag, "sha256:") {
		serveManifest(w, r, h.image.BlobsPath(tag))
		return
	}

	manifestPath := h.image.ManifestPath(image, tag)
	_, err := os.Stat(manifestPath)
	if err != nil {
		err := h.build(image, tag)
		if err != nil {
			slog.Error("image.Build", "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	serveManifest(w, r, h.image.ManifestPath(image, tag))
}

func (h *Handler) build(image, tag string) error {
	ref := image + ":" + tag

	mut, ok := h.buildMutex.LoadOrStore(ref, &sync.RWMutex{})
	if ok {
		mut.RLock()
		defer mut.RUnlock()
		return nil
	}

	mut.Lock()
	defer func() {
		h.buildMutex.Delete(ref)
		mut.Unlock()
	}()

	rules := h.getRules()
	for _, rule := range rules {
		mutates, ok := rule.Match(ref)
		if ok {
			err := h.image.Build(ref, mutates)
			if err != nil {
				return err
			}
			break
		}
	}
	return nil
}

func serveManifest(w http.ResponseWriter, r *http.Request, manifestPath string) {
	f, err := os.Open(manifestPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	mediaType := struct {
		MediaType types.MediaType `json:"mediaType,omitempty"`
	}{}

	err = json.NewDecoder(f).Decode(&mediaType)
	if err != nil {
		slog.Error("json.Decode", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stat, err := f.Stat()
	if err != nil {
		slog.Error("Stat", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", string(mediaType.MediaType))
	_, _ = f.Seek(0, 0)
	http.ServeContent(w, r, path.Base(r.URL.Path), stat.ModTime(), f)
}
