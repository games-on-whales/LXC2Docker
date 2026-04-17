package api

import (
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/lxc"
	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
	"github.com/gorilla/mux"
)

// Handler is the root HTTP handler. It holds references to the LXC manager
// and the metadata store, and owns the in-memory exec instance table.
type Handler struct {
	mgr        *lxc.Manager
	store      *store.Store
	execs      *execStore
	eventsHub  *eventHub
	attachMu   sync.Mutex
	attachPTYs map[string]*os.File
}

// NewHandler wires up the Handler and returns an http.Handler ready to serve.
func NewHandler(mgr *lxc.Manager, st *store.Store) http.Handler {
	h := &Handler{
		mgr:        mgr,
		store:      st,
		execs:      newExecStore(),
		eventsHub:  newEventHub(),
		attachPTYs: make(map[string]*os.File),
	}
	// Periodically prune completed exec records to prevent memory leaks.
	go func() {
		for {
			time.Sleep(60 * time.Second)
			h.execs.prune()
		}
	}()
	return h.routes()
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
		sub.HandleFunc("/events", h.events).Methods(http.MethodGet)

		// Networks (stub)
		sub.HandleFunc("/networks", h.listNetworks).Methods(http.MethodGet)
		sub.HandleFunc("/networks/{id}", h.inspectNetwork).Methods(http.MethodGet)
		sub.HandleFunc("/networks/{id}", h.removeNetwork).Methods(http.MethodDelete)
		sub.HandleFunc("/networks/create", h.createNetwork).Methods(http.MethodPost)
		sub.HandleFunc("/networks/prune", h.pruneNetworks).Methods(http.MethodPost)
		sub.HandleFunc("/networks/{id}/connect", h.connectNetwork).Methods(http.MethodPost)
		sub.HandleFunc("/networks/{id}/disconnect", h.disconnectNetwork).Methods(http.MethodPost)
		sub.HandleFunc("/system/df", h.systemDiskUsage).Methods(http.MethodGet)
		sub.HandleFunc("/build/prune", h.pruneBuildCache).Methods(http.MethodPost)
		sub.HandleFunc("/distribution/{name:.*}/json", h.inspectDistribution).Methods(http.MethodGet)
		sub.HandleFunc("/auth", h.auth).Methods(http.MethodPost)
		sub.HandleFunc("/plugins", h.listPlugins).Methods(http.MethodGet)

		// Swarm-mode endpoints. Docker returns 503 "This node is not a swarm
		// manager" when swarm isn't initialised; Portainer treats that as
		// "swarm unavailable" and hides the feature. A 404 instead is
		// surfaced as a scary error banner.
		sub.HandleFunc("/swarm", h.swarmUnavailable).Methods(http.MethodGet)
		sub.HandleFunc("/swarm/{rest:.*}", h.swarmUnavailable)
		sub.HandleFunc("/nodes", h.swarmUnavailable).Methods(http.MethodGet)
		sub.HandleFunc("/nodes/{rest:.*}", h.swarmUnavailable)
		sub.HandleFunc("/services", h.swarmUnavailable).Methods(http.MethodGet)
		sub.HandleFunc("/services/{rest:.*}", h.swarmUnavailable)
		sub.HandleFunc("/tasks", h.swarmUnavailable).Methods(http.MethodGet)
		sub.HandleFunc("/tasks/{rest:.*}", h.swarmUnavailable)
		sub.HandleFunc("/configs", h.swarmUnavailable).Methods(http.MethodGet)
		sub.HandleFunc("/configs/{rest:.*}", h.swarmUnavailable)
		sub.HandleFunc("/secrets", h.swarmUnavailable).Methods(http.MethodGet)
		sub.HandleFunc("/secrets/{rest:.*}", h.swarmUnavailable)

		// Containers
		sub.HandleFunc("/containers/json", h.listContainers).Methods(http.MethodGet)
		sub.HandleFunc("/containers/create", h.createContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/json", h.inspectContainer).Methods(http.MethodGet)
		sub.HandleFunc("/containers/{id}/stats", h.containerStats).Methods(http.MethodGet)
		sub.HandleFunc("/containers/{id}/changes", h.containerChanges).Methods(http.MethodGet)
		sub.HandleFunc("/containers/{id}/start", h.startContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/stop", h.stopContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/kill", h.killContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/wait", h.waitContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/restart", h.restartContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/rename", h.renameContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/top", h.topContainer).Methods(http.MethodGet)
		sub.HandleFunc("/containers/{id}/attach", h.attachContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/logs", h.containerLogs).Methods(http.MethodGet)
		sub.HandleFunc("/containers/{id}/archive", h.putArchive).Methods(http.MethodPut)
		sub.HandleFunc("/containers/{id}/archive", h.getArchive).Methods(http.MethodGet, http.MethodHead)
		sub.HandleFunc("/containers/{id}/resize", h.resizeContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/pause", h.pauseContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/unpause", h.unpauseContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}/update", h.updateContainer).Methods(http.MethodPost)
		sub.HandleFunc("/containers/prune", h.pruneContainers).Methods(http.MethodPost)
		sub.HandleFunc("/containers/{id}", h.removeContainer).Methods(http.MethodDelete)

		// Images
		sub.HandleFunc("/images/json", h.listImages).Methods(http.MethodGet)
		sub.HandleFunc("/images/create", h.pullImage).Methods(http.MethodPost)
		sub.HandleFunc("/build", h.buildImage).Methods(http.MethodPost)
		sub.HandleFunc("/images/search", h.searchImages).Methods(http.MethodGet)
		sub.HandleFunc("/images/{name:.*}/json", h.inspectImage).Methods(http.MethodGet)
		sub.HandleFunc("/images/{name:.*}/history", h.imageHistory).Methods(http.MethodGet)
		sub.HandleFunc("/images/{name:.*}/tag", h.tagImage).Methods(http.MethodPost)
		sub.HandleFunc("/images/{name:.*}/push", h.pushImage).Methods(http.MethodPost)
		sub.HandleFunc("/images/prune", h.pruneImages).Methods(http.MethodPost)
		sub.HandleFunc("/images/{name:.*}", h.removeImage).Methods(http.MethodDelete)
		sub.HandleFunc("/commit", h.commitContainer).Methods(http.MethodPost)

		// Volumes
		sub.HandleFunc("/volumes", h.listVolumes).Methods(http.MethodGet)
		sub.HandleFunc("/volumes/create", h.createVolume).Methods(http.MethodPost)
		sub.HandleFunc("/volumes/prune", h.pruneVolumes).Methods(http.MethodPost)
		sub.HandleFunc("/volumes/{name}", h.inspectVolume).Methods(http.MethodGet)
		sub.HandleFunc("/volumes/{name}", h.removeVolume).Methods(http.MethodDelete)

		// Exec
		sub.HandleFunc("/containers/{id}/exec", h.execCreate).Methods(http.MethodPost)
		sub.HandleFunc("/exec/{id}/start", h.execStart).Methods(http.MethodPost)
		sub.HandleFunc("/exec/{id}/json", h.execInspect).Methods(http.MethodGet)
		sub.HandleFunc("/exec/{id}/resize", h.execResize).Methods(http.MethodPost)
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
