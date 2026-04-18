package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
	"github.com/gorilla/mux"
)

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

	r := h.routes()
	req := httptest.NewRequest(http.MethodPost, "/v1.45/images/load", nil)
	match := &mux.RouteMatch{}
	if !r.Match(req, match) {
		t.Fatal("expected /images/load route to match")
	}

	got, ok := match.Handler.(http.HandlerFunc)
	if !ok {
		t.Fatalf("expected route handler to be http.HandlerFunc, got %T", match.Handler)
	}

	want := http.HandlerFunc(h.loadImage)
	if reflect.ValueOf(got).Pointer() != reflect.ValueOf(want).Pointer() {
		t.Fatalf("expected /images/load to map to loadImage handler")
	}
}

func TestImagesGetSingleRouteHitsSaveImageHandler(t *testing.T) {
	t.Parallel()

	h := &Handler{
		attachPTYs: map[string]*os.File{},
		events:     newEventBroker(),
	}

	r := h.routes()
	req := httptest.NewRequest(http.MethodGet, "/v1.45/images/nginx:latest/get", nil)
	match := &mux.RouteMatch{}
	if !r.Match(req, match) {
		t.Fatal("expected /images/{name}/get route to match")
	}

	got, ok := match.Handler.(http.HandlerFunc)
	if !ok {
		t.Fatalf("expected route handler to be http.HandlerFunc, got %T", match.Handler)
	}

	want := http.HandlerFunc(h.saveImage)
	if reflect.ValueOf(got).Pointer() != reflect.ValueOf(want).Pointer() {
		t.Fatalf("expected /images/{name}/get to map to saveImage handler")
	}
}

func TestImagesGetBulkRouteHitsSaveImagesHandler(t *testing.T) {
	t.Parallel()

	h := &Handler{
		attachPTYs: map[string]*os.File{},
		events:     newEventBroker(),
	}

	r := h.routes()
	req := httptest.NewRequest(http.MethodGet, "/v1.45/images/get", nil)
	match := &mux.RouteMatch{}
	if !r.Match(req, match) {
		t.Fatal("expected /images/get route to match")
	}

	got, ok := match.Handler.(http.HandlerFunc)
	if !ok {
		t.Fatalf("expected route handler to be http.HandlerFunc, got %T", match.Handler)
	}

	want := http.HandlerFunc(h.saveImages)
	if reflect.ValueOf(got).Pointer() != reflect.ValueOf(want).Pointer() {
		t.Fatalf("expected /images/get to map to saveImages handler")
	}
}

func TestImagesSearchRouteHitsSearchImagesHandler(t *testing.T) {
	t.Parallel()

	h := &Handler{
		store:      nil,
		attachPTYs: map[string]*os.File{},
		events:     newEventBroker(),
	}

	r := h.routes()
	req := httptest.NewRequest(http.MethodGet, "/v1.45/images/search?term=nginx", nil)
	match := &mux.RouteMatch{}
	if !r.Match(req, match) {
		t.Fatal("expected /images/search route to match")
	}

	got, ok := match.Handler.(http.HandlerFunc)
	if !ok {
		t.Fatalf("expected route handler to be http.HandlerFunc, got %T", match.Handler)
	}

	want := http.HandlerFunc(h.searchImages)
	if reflect.ValueOf(got).Pointer() != reflect.ValueOf(want).Pointer() {
		t.Fatalf("expected /images/search to map to searchImages handler")
	}
}

func TestContainerExportRouteHitsExportHandler(t *testing.T) {
	t.Parallel()

	h := &Handler{
		store:      nil,
		attachPTYs: map[string]*os.File{},
		events:     newEventBroker(),
	}

	r := h.routes()
	req := httptest.NewRequest(http.MethodGet, "/v1.45/containers/abc/export", nil)
	match := &mux.RouteMatch{}
	if !r.Match(req, match) {
		t.Fatal("expected /containers/{id}/export route to match")
	}

	got, ok := match.Handler.(http.HandlerFunc)
	if !ok {
		t.Fatalf("expected route handler to be http.HandlerFunc, got %T", match.Handler)
	}

	want := http.HandlerFunc(h.exportContainer)
	if reflect.ValueOf(got).Pointer() != reflect.ValueOf(want).Pointer() {
		t.Fatalf("expected /containers/{id}/export to map to exportContainer handler")
	}
}

func TestBuildRoutesHitBuildHandlers(t *testing.T) {
	t.Parallel()

	h := &Handler{
		attachPTYs: map[string]*os.File{},
		events:     newEventBroker(),
	}

	r := h.routes()

	buildReq := httptest.NewRequest(http.MethodPost, "/v1.45/build?t=foo", nil)
	buildMatch := &mux.RouteMatch{}
	if !r.Match(buildReq, buildMatch) {
		t.Fatal("expected /build route to match")
	}
	buildGot, ok := buildMatch.Handler.(http.HandlerFunc)
	if !ok {
		t.Fatalf("expected /build handler to be http.HandlerFunc, got %T", buildMatch.Handler)
	}
	wantBuild := http.HandlerFunc(h.buildImage)
	if reflect.ValueOf(buildGot).Pointer() != reflect.ValueOf(wantBuild).Pointer() {
		t.Fatalf("expected /build to map to buildImage handler")
	}

	pruneReq := httptest.NewRequest(http.MethodPost, "/v1.45/build/prune", nil)
	pruneMatch := &mux.RouteMatch{}
	if !r.Match(pruneReq, pruneMatch) {
		t.Fatal("expected /build/prune route to match")
	}
	pruneGot, ok := pruneMatch.Handler.(http.HandlerFunc)
	if !ok {
		t.Fatalf("expected /build/prune handler to be http.HandlerFunc, got %T", pruneMatch.Handler)
	}
	wantPrune := http.HandlerFunc(h.pruneBuildCache)
	if reflect.ValueOf(pruneGot).Pointer() != reflect.ValueOf(wantPrune).Pointer() {
		t.Fatalf("expected /build/prune to map to pruneBuildCache handler")
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
	if len(out) != 1 {
		t.Fatalf("expected one match, got %d: %#v", len(out), out)
	}
	if out[0].Name != "nginx" {
		t.Fatalf("expected match name nginx, got %#v", out[0].Name)
	}
}
