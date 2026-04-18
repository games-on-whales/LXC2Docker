package api

import (
	"encoding/json"
	"os"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/lxc"
	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
	"github.com/gorilla/mux"
)

// Handler is the root HTTP handler. It holds references to the LXC manager
// and the metadata store, owns the in-memory exec instance table, and fans
// out lifecycle events to /events subscribers.
type Handler struct {
	mgr    *lxc.Manager
	store  *store.Store
	execs  *execStore
	events *eventBroker
	attachMu  sync.Mutex
	attachPTYs map[string]*os.File
}

// NewHandler wires up the Handler and returns an http.Handler ready to serve.
func NewHandler(mgr *lxc.Manager, st *store.Store) http.Handler {
	return newHandler(mgr, st).routes()
}

// NewHandlerWithHooks is like NewHandler but also returns the Handler so the
// caller can wire hooks that depend on the event broker (e.g. health
// watcher → events). Returning the concrete *Handler would leak internals
// to main.go; instead we expose only the hook type main.go needs.
func NewHandlerWithHooks(mgr *lxc.Manager, st *store.Store) (http.Handler, func(id, status string), func(id, action string)) {
	h := newHandler(mgr, st)
	return h.routes(), h.HealthEmitter(), h.RestartEmitter()
}

func newHandler(mgr *lxc.Manager, st *store.Store) *Handler {
	h := &Handler{
		mgr:    mgr,
		store:  st,
		execs:  newExecStore(),
		events: newEventBroker(),
		attachPTYs: make(map[string]*os.File),
	}
	// Periodically prune completed exec records to prevent memory leaks.
	go func() {
		for {
			time.Sleep(60 * time.Second)
			h.execs.prune()
		}
	}()
	return h
}

func (h *Handler) routes() http.Handler {
	r := mux.NewRouter()

	// Docker clients prefix all paths with /v<version>/. We accept any version
	// prefix by using a subrouter that strips it, and also mount the bare paths
	// so that clients that omit the version still work.
	api := r.PathPrefix("/v{version:[0-9.]+}").Subrouter()

	for _, sub := range []*mux.Router{r, api} {
		// System
		sub.HandleFunc("/_ping", h.ping).Methods(http.MethodGet, http.MethodHead)
		sub.HandleFunc("/version", h.version).Methods(http.MethodGet)
		sub.HandleFunc("/info", h.info).Methods(http.MethodGet)
		sub.HandleFunc("/events", h.eventsStream).Methods(http.MethodGet)
		sub.HandleFunc("/system/df", h.systemDF).Methods(http.MethodGet)
		sub.HandleFunc("/auth", h.auth).Methods(http.MethodPost)

		// Networks
		sub.HandleFunc("/networks", h.listNetworks).Methods(http.MethodGet)
		sub.HandleFunc("/networks/create", h.createNetwork).Methods(http.MethodPost)
		sub.HandleFunc("/networks/prune", h.pruneNetworks).Methods(http.MethodPost)
		sub.HandleFunc("/networks/{id}", h.inspectNetwork).Methods(http.MethodGet)
		sub.HandleFunc("/networks/{id}", h.removeNetwork).Methods(http.MethodDelete)
		sub.HandleFunc("/networks/{id}/connect", h.connectNetwork).Methods(http.MethodPost)
		sub.HandleFunc("/networks/{id}/disconnect", h.disconnectNetwork).Methods(http.MethodPost)

		// Volumes (stubs — the daemon uses bind mounts, but Portainer polls)
		sub.HandleFunc("/volumes", h.listVolumes).Methods(http.MethodGet)
		sub.HandleFunc("/volumes/create", h.createVolume).Methods(http.MethodPost)
		sub.HandleFunc("/volumes/prune", h.pruneVolumes).Methods(http.MethodPost)
		sub.HandleFunc("/volumes/{name}", h.inspectVolume).Methods(http.MethodGet)
		sub.HandleFunc("/volumes/{name}", h.removeVolume).Methods(http.MethodDelete)

		// Containers
		sub.HandleFunc("/containers/json", h.listContainers).Methods(http.MethodGet)
		sub.HandleFunc("/containers/create", h.createContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/prune", h.pruneContainers).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/json", h.inspectContainer).Methods(http.MethodGet)
		sub.HandleFunc("/containers/{id}/start", h.startContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/stop", h.stopContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/kill", h.killContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/wait", h.waitContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/restart", h.restartContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/rename", h.renameContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/update", h.updateContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/pause", h.pauseContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/unpause", h.unpauseContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/top", h.topContainer).Methods(http.MethodGet)
		sub.HandleFunc("/containers/{id}/stats", h.containerStats).Methods(http.MethodGet)
		sub.HandleFunc("/containers/{id}/changes", h.containerChanges).Methods(http.MethodGet)
		sub.HandleFunc("/containers/{id}/resize", h.resizeContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/attach", h.attachContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/logs", h.containerLogs).Methods(http.MethodGet)
		sub.HandleFunc("/containers/{id}/archive", h.putArchive).Methods(http.MethodPut)
		sub.HandleFunc("/containers/{id}/archive", h.getArchive).Methods(http.MethodGet, http.MethodHead)
		sub.HandleFunc("/containers/{id}", h.removeContainer).Methods(http.MethodDelete)

		// Images
		sub.HandleFunc("/images/json", h.listImages).Methods(http.MethodGet)
		sub.HandleFunc("/images/create", h.pullImage).Methods(http.MethodPost)
		sub.HandleFunc("/images/search", h.searchImages).Methods(http.MethodGet)
		sub.HandleFunc("/images/{name:.*}/get", h.saveImage).Methods(http.MethodGet)
		sub.HandleFunc("/images/get", h.saveImages).Methods(http.MethodGet)
		sub.HandleFunc("/images/prune", h.pruneImages).Methods(http.MethodPost)
		sub.HandleFunc("/images/{name:.*}/json", h.inspectImage).Methods(http.MethodGet, http.MethodHead)
		sub.HandleFunc("/images/{name:.*}/history", h.imageHistory).Methods(http.MethodGet)
		sub.HandleFunc("/images/{name:.*}/tag", h.tagImage).Methods(http.MethodPost)
		sub.HandleFunc("/images/{name:.*}", h.removeImage).Methods(http.MethodDelete)
		sub.HandleFunc("/containers/{id}/export", h.exportContainer).Methods(http.MethodGet)

		// Distribution (registry manifest probe used by Portainer's pull modal)
		sub.HandleFunc("/distribution/{name:.*}/json", h.distributionInspect).Methods(http.MethodGet)

		// Polite 501s for engine features that this daemon doesn't implement.
		// Portainer surfaces these as UI tabs; a structured error beats a
		// 404 that litters the browser console and confuses users. The
		// messages are short because Portainer displays them verbatim.
		ni := notImplementedFunc("not supported by docker-lxc-daemon")
		sub.HandleFunc("/build", h.buildImage).Methods(http.MethodPost)
		sub.HandleFunc("/build/prune", h.pruneBuildCache).Methods(http.MethodPost)
		sub.HandleFunc("/images/load", h.loadImage).Methods(http.MethodPost)
		sub.HandleFunc("/commit", h.commitContainer).Methods(http.MethodPost)
		sub.HandleFunc("/session", ni).Methods(http.MethodPost)
		sub.HandleFunc("/plugins", h.listPlugins).Methods(http.MethodGet)
		sub.HandleFunc("/swarm", ni).Methods(http.MethodGet)
		sub.HandleFunc("/swarm/init", ni).Methods(http.MethodPost)
		sub.HandleFunc("/swarm/join", ni).Methods(http.MethodPost)
		sub.HandleFunc("/swarm/leave", ni).Methods(http.MethodPost)
		sub.HandleFunc("/nodes", ni).Methods(http.MethodGet)
		sub.HandleFunc("/services", ni).Methods(http.MethodGet)
		sub.HandleFunc("/tasks", ni).Methods(http.MethodGet)
		sub.HandleFunc("/configs", ni).Methods(http.MethodGet)
		sub.HandleFunc("/secrets", ni).Methods(http.MethodGet)

		// Exec
		sub.HandleFunc("/containers/{id}/exec", h.execCreate).Methods(http.MethodPost)
		sub.HandleFunc("/exec/{id}/start", h.execStart).Methods(http.MethodPost)
		sub.HandleFunc("/exec/{id}/resize", h.resizeExec).Methods(http.MethodPost)
		sub.HandleFunc("/exec/{id}/json", h.execInspect).Methods(http.MethodGet)
	}

	// Log all requests for debugging.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			log.Printf("API: %s %s", req.Method, req.URL.Path)
			rw := &statusRecorder{ResponseWriter: w, code: 200}
			next.ServeHTTP(rw, req)
			log.Printf("API: %s %s → %d", req.Method, req.URL.Path, rw.code)
		})
	})
	// Catch-all for unmatched routes so we log 404s with the path.
	r.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		log.Printf("API: 404 not found: %s %s", req.Method, req.URL.Path)
		errResponse(w, http.StatusNotFound, "404 page not found")
	})

	return r
}

// notImplementedFunc returns an HTTP handler that responds 501 with a
// structured error body. Used for Docker Engine endpoints that Portainer
// probes but this daemon has no analog for (build, swarm, services, …).
// 501 is the spec-correct code — it tells clients the feature is missing
// rather than the path being wrong, which prevents retry storms and
// surfaces a clearer message in Portainer's console.
func notImplementedFunc(msg string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		errResponse(w, http.StatusNotImplemented, msg)
	}
}

func buildNotImplemented(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	msg := "image build is not supported by docker-lxc-daemon; use `docker pull` or the GoW image registry"
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errorDetail": map[string]string{"message": msg},
		"error":       msg,
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
