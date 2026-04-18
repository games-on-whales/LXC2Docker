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
