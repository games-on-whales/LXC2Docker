package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"testing"

	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
	"github.com/gorilla/mux"
)

func mustMuxRouter(t *testing.T, h http.Handler) *mux.Router {
	t.Helper()
	r, ok := h.(*mux.Router)
	if !ok {
		t.Fatalf("expected *mux.Router, got %T", h)
	}
	return r
}

func TestDistributionInspectUsesCanonicalPayload(t *testing.T) {
	t.Parallel()

	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store init: %v", err)
	}
	if err := st.AddImage(&store.ImageRecord{
		ID:  "sha256image",
		Ref: "docker.io/library/nginx:latest",
	}); err != nil {
		t.Fatalf("add image: %v", err)
	}

	h := &Handler{
		store:      st,
		attachPTYs: map[string]*os.File{},
		execs:      newExecStore(),
		events:     newEventBroker(),
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1.45/distribution/nginx:latest/json", nil)
	req = mux.SetURLVars(req, map[string]string{"name": "nginx:latest"})
	h.distributionInspect(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var out map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	descRaw, ok := out["Descriptor"].(map[string]any)
	if !ok {
		t.Fatalf("Descriptor missing or wrong type: %#v", out["Descriptor"])
	}
	if _, ok := descRaw["mediaType"]; !ok {
		t.Fatalf("missing descriptor mediaType key: %#v", out)
	}
	if _, ok := descRaw["digest"]; !ok {
		t.Fatalf("missing descriptor digest key: %#v", out)
	}
	if _, ok := descRaw["size"]; !ok {
		t.Fatalf("missing descriptor size key: %#v", out)
	}
}

func TestImagesLoadRouteHitsLoadImageHandler(t *testing.T) {
	t.Parallel()

	h := &Handler{
		attachPTYs: map[string]*os.File{},
		events:     newEventBroker(),
	}

	r := mustMuxRouter(t, h.routes())
	req := httptest.NewRequest(http.MethodPost, "/v1.45/images/load", nil)
	match := &mux.RouteMatch{}
	if !r.Match(req, match) {
		t.Fatal("expected /images/load route to match")
	}
}

func TestImagesGetSingleRouteHitsSaveImageHandler(t *testing.T) {
	t.Parallel()

	h := &Handler{
		attachPTYs: map[string]*os.File{},
		events:     newEventBroker(),
	}

	r := mustMuxRouter(t, h.routes())
	req := httptest.NewRequest(http.MethodGet, "/v1.45/images/nginx:latest/get", nil)
	match := &mux.RouteMatch{}
	if !r.Match(req, match) {
		t.Fatal("expected /images/{name}/get route to match")
	}
}

func TestImagesGetBulkRouteHitsSaveImagesHandler(t *testing.T) {
	t.Parallel()

	h := &Handler{
		attachPTYs: map[string]*os.File{},
		events:     newEventBroker(),
	}

	r := mustMuxRouter(t, h.routes())
	req := httptest.NewRequest(http.MethodGet, "/v1.45/images/get", nil)
	match := &mux.RouteMatch{}
	if !r.Match(req, match) {
		t.Fatal("expected /images/get route to match")
	}
}

func TestImagesSearchRouteHitsSearchImagesHandler(t *testing.T) {
	t.Parallel()

	h := &Handler{
		store:      nil,
		attachPTYs: map[string]*os.File{},
		events:     newEventBroker(),
	}

	r := mustMuxRouter(t, h.routes())
	req := httptest.NewRequest(http.MethodGet, "/v1.45/images/search?term=nginx", nil)
	match := &mux.RouteMatch{}
	if !r.Match(req, match) {
		t.Fatal("expected /images/search route to match")
	}
}

func TestContainerExportRouteHitsExportHandler(t *testing.T) {
	t.Parallel()

	h := &Handler{
		store:      nil,
		attachPTYs: map[string]*os.File{},
		events:     newEventBroker(),
	}

	r := mustMuxRouter(t, h.routes())
	req := httptest.NewRequest(http.MethodGet, "/v1.45/containers/abc/export", nil)
	match := &mux.RouteMatch{}
	if !r.Match(req, match) {
		t.Fatal("expected /containers/{id}/export route to match")
	}
}

func TestBuildRoutesHitBuildHandlers(t *testing.T) {
	t.Parallel()

	h := &Handler{
		attachPTYs: map[string]*os.File{},
		events:     newEventBroker(),
	}

	r := mustMuxRouter(t, h.routes())

	buildReq := httptest.NewRequest(http.MethodPost, "/v1.45/build?t=foo", nil)
	buildMatch := &mux.RouteMatch{}
	if !r.Match(buildReq, buildMatch) {
		t.Fatal("expected /build route to match")
	}

	pruneReq := httptest.NewRequest(http.MethodPost, "/v1.45/build/prune", nil)
	pruneMatch := &mux.RouteMatch{}
	if !r.Match(pruneReq, pruneMatch) {
		t.Fatal("expected /build/prune route to match")
	}
}

func TestSwarmRoutesReturnSwarmUnavailable(t *testing.T) {
	t.Parallel()

	h := &Handler{
		attachPTYs: map[string]*os.File{},
		events:     newEventBroker(),
	}

	r := mustMuxRouter(t, h.routes())
	tests := []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/v1.45/swarm"},
		{method: http.MethodHead, path: "/v1.45/swarm"},
		{method: http.MethodPost, path: "/v1.45/swarm/init"},
		{method: http.MethodPost, path: "/v1.45/swarm/join"},
		{method: http.MethodPost, path: "/v1.45/swarm/leave"},
		{method: http.MethodGet, path: "/v1.45/swarm/unlockkey"},
		{method: http.MethodHead, path: "/v1.45/swarm/unlockkey"},
		{method: http.MethodGet, path: "/v1.45/swarm/join-token"},
		{method: http.MethodHead, path: "/v1.45/swarm/join-token"},
		{method: http.MethodPost, path: "/v1.45/swarm/update"},
		{method: http.MethodGet, path: "/v1.45/nodes"},
		{method: http.MethodHead, path: "/v1.45/nodes"},
		{method: http.MethodGet, path: "/v1.45/nodes/node-1"},
		{method: http.MethodHead, path: "/v1.45/nodes/node-1"},
		{method: http.MethodPost, path: "/v1.45/nodes/node-1/update"},
		{method: http.MethodGet, path: "/v1.45/nodes/node-1/tasks"},
		{method: http.MethodHead, path: "/v1.45/nodes/node-1/tasks"},
		{method: http.MethodGet, path: "/v1.45/services"},
		{method: http.MethodHead, path: "/v1.45/services"},
		{method: http.MethodPost, path: "/v1.45/services/create"},
		{method: http.MethodGet, path: "/v1.45/services/service-1"},
		{method: http.MethodHead, path: "/v1.45/services/service-1"},
		{method: http.MethodDelete, path: "/v1.45/services/service-1"},
		{method: http.MethodPost, path: "/v1.45/services/service-1/update"},
		{method: http.MethodGet, path: "/v1.45/services/service-1/logs"},
		{method: http.MethodHead, path: "/v1.45/services/service-1/logs"},
		{method: http.MethodGet, path: "/v1.45/services/service-1/tasks"},
		{method: http.MethodHead, path: "/v1.45/services/service-1/tasks"},
		{method: http.MethodGet, path: "/v1.45/tasks"},
		{method: http.MethodHead, path: "/v1.45/tasks"},
		{method: http.MethodGet, path: "/v1.45/tasks/task-1"},
		{method: http.MethodHead, path: "/v1.45/tasks/task-1"},
		{method: http.MethodGet, path: "/v1.45/tasks/task-1/logs"},
		{method: http.MethodHead, path: "/v1.45/tasks/task-1/logs"},
		{method: http.MethodGet, path: "/v1.45/configs"},
		{method: http.MethodHead, path: "/v1.45/configs"},
		{method: http.MethodPost, path: "/v1.45/configs/create"},
		{method: http.MethodGet, path: "/v1.45/configs/config-1"},
		{method: http.MethodDelete, path: "/v1.45/configs/config-1"},
		{method: http.MethodPost, path: "/v1.45/configs/config-1/update"},
		{method: http.MethodGet, path: "/v1.45/secrets"},
		{method: http.MethodHead, path: "/v1.45/secrets"},
		{method: http.MethodPost, path: "/v1.45/secrets/create"},
		{method: http.MethodGet, path: "/v1.45/secrets/secret-1"},
		{method: http.MethodDelete, path: "/v1.45/secrets/secret-1"},
		{method: http.MethodPost, path: "/v1.45/secrets/secret-1/update"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503 for %s %s, got %d body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestSearchImagesFiltersLocalCatalog(t *testing.T) {
	t.Parallel()

	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store init: %v", err)
	}
	if err := st.AddImage(&store.ImageRecord{ID: "sha1", Ref: "docker.io/library/nginx:latest"}); err != nil {
		t.Fatalf("add image: %v", err)
	}
	if err := st.AddImage(&store.ImageRecord{ID: "sha2", Ref: "docker.io/library/busybox:latest"}); err != nil {
		t.Fatalf("add image: %v", err)
	}
	if err := st.AddImage(&store.ImageRecord{ID: "sha3", Ref: "docker.io/library/nginx:alpine"}); err != nil {
		t.Fatalf("add image: %v", err)
	}

	h := &Handler{
		store:      st,
		attachPTYs: map[string]*os.File{},
		execs:      newExecStore(),
		events:     newEventBroker(),
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1.45/images/search?term=nginx", nil)
	h.searchImages(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var out []ImageSearchResult
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected two matches, got %d: %#v", len(out), out)
	}
	names := []string{out[0].Name, out[1].Name}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"nginx:alpine", "nginx:latest"}) {
		t.Fatalf("expected nginx tags in search results, got %#v", names)
	}
}

func TestHeadMethodsRouteToDockerReadHandlers(t *testing.T) {
	t.Parallel()

	h := &Handler{
		attachPTYs: map[string]*os.File{},
		events:     newEventBroker(),
	}

	r := mustMuxRouter(t, h.routes())
	tests := []struct {
		name string
		path string
	}{
		{name: "version", path: "/v1.45/version"},
		{name: "info", path: "/v1.45/info"},
		{name: "events", path: "/v1.45/events"},
		{name: "system df", path: "/v1.45/system/df"},
		{name: "networks", path: "/v1.45/networks"},
		{name: "inspect network", path: "/v1.45/networks/net-1"},
		{name: "volumes", path: "/v1.45/volumes"},
		{name: "inspect volume", path: "/v1.45/volumes/my-volume"},
		{name: "containers", path: "/v1.45/containers/json"},
		{name: "inspect container", path: "/v1.45/containers/abc/json"},
		{name: "images", path: "/v1.45/images/json"},
		{name: "search images", path: "/v1.45/images/search?term=nginx"},
		{name: "inspect image", path: "/v1.45/images/ubuntu/json"},
		{name: "save image", path: "/v1.45/images/ubuntu/get"},
		{name: "save images", path: "/v1.45/images/get"},
		{name: "image history", path: "/v1.45/images/ubuntu/history"},
		{name: "distribution inspect", path: "/v1.45/distribution/ubuntu/json"},
		{name: "container export", path: "/v1.45/containers/abc/export"},
		{name: "container top", path: "/v1.45/containers/abc/top"},
		{name: "container stats", path: "/v1.45/containers/abc/stats"},
		{name: "container changes", path: "/v1.45/containers/abc/changes"},
		{name: "container logs", path: "/v1.45/containers/abc/logs"},
		{name: "exec inspect", path: "/v1.45/exec/abc/json"},
		{name: "plugins", path: "/v1.45/plugins"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodHead, tc.path, nil)
			match := &mux.RouteMatch{}
			if !r.Match(req, match) {
				t.Fatalf("expected %s route to match", tc.path)
			}
		})
	}
}

func TestAdditionalPortainerRoutesExist(t *testing.T) {
	t.Parallel()

	h := &Handler{
		store:      nil,
		attachPTYs: map[string]*os.File{},
		events:     newEventBroker(),
	}
	r := mustMuxRouter(t, h.routes())

	tests := []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/v1.45/containers/abc/attach/ws"},
		{method: http.MethodPost, path: "/v1.45/images/nginx:latest/push"},
		{method: http.MethodPost, path: "/v1.45/swarm/unlock"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			t.Parallel()
			match := &mux.RouteMatch{}
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if !r.Match(req, match) {
				t.Fatalf("expected %s route to match", tc.path)
			}
		})
	}
}

func TestSessionRouteIsExplicitlyNotImplemented(t *testing.T) {
	t.Parallel()

	h := &Handler{
		attachPTYs: map[string]*os.File{},
		events:     newEventBroker(),
	}
	r := mustMuxRouter(t, h.routes())

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(method, "/v1.45/session", nil)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)
			if rr.Code != http.StatusNotImplemented {
				t.Fatalf("expected 501 for %s /session, got %d body=%s", method, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestUpdateContainerPersistsPortainerResourceChanges(t *testing.T) {
	t.Parallel()

	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store init: %v", err)
	}

	initialHC := HostConfig{
		Memory:    128 * 1024 * 1024,
		CPUShares: 128,
		RestartPolicy: RestartPolicy{
			Name:              "no",
			MaximumRetryCount: 0,
		},
		OomScoreAdj: 250,
	}
	rawHC, err := json.Marshal(initialHC)
	if err != nil {
		t.Fatalf("marshal initial host config: %v", err)
	}

	const id = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"
	if err := st.AddContainer(&store.ContainerRecord{
		ID:              id,
		Name:            "web",
		Image:           "docker.io/library/nginx:latest",
		RawHostConfig:   rawHC,
		RestartPolicy:   "no",
		RestartMaxRetry: 0,
		OomScoreAdj:     250,
	}); err != nil {
		t.Fatalf("add container: %v", err)
	}

	h := &Handler{
		store:      st,
		attachPTYs: map[string]*os.File{},
		execs:      newExecStore(),
		events:     newEventBroker(),
	}

	body := []byte(`{
		"Memory": 268435456,
		"CpuShares": 512,
		"RestartPolicy": {"Name":"unless-stopped","MaximumRetryCount":0},
		"OomScoreAdj": 0
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1.45/containers/"+id+"/update", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"id": id})
	rr := httptest.NewRecorder()
	h.updateContainer(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Warnings []string `json:"Warnings"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Warnings) != 0 {
		t.Fatalf("expected no warnings, got %#v", resp.Warnings)
	}

	updated := st.GetContainer(id)
	if updated == nil {
		t.Fatal("updated container missing from store")
	}
	if updated.RestartPolicy != "unless-stopped" {
		t.Fatalf("expected restart policy to persist, got %q", updated.RestartPolicy)
	}
	if updated.OomScoreAdj != 0 {
		t.Fatalf("expected oom_score_adj to persist, got %d", updated.OomScoreAdj)
	}

	hc := buildHostConfig(updated)
	if hc.Memory != 268435456 {
		t.Fatalf("expected Memory=268435456, got %d", hc.Memory)
	}
	if hc.CPUShares != 512 {
		t.Fatalf("expected CpuShares=512, got %d", hc.CPUShares)
	}
	if hc.RestartPolicy.Name != "unless-stopped" {
		t.Fatalf("expected RestartPolicy.Name=unless-stopped, got %q", hc.RestartPolicy.Name)
	}
	if hc.OomScoreAdj != 0 {
		t.Fatalf("expected OomScoreAdj=0, got %d", hc.OomScoreAdj)
	}
}
