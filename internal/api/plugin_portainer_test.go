package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
	"github.com/gorilla/mux"
)

func TestPluginRoutesExistForPortainer(t *testing.T) {
	t.Parallel()

	h := &Handler{
		attachPTYs: map[string]*os.File{},
		events:     newEventBroker(),
	}
	r := mustMuxRouter(t, h.routes())

	tests := []struct {
		method string
		path   string
		status int
	}{
		{method: http.MethodGet, path: "/v1.45/plugins", status: http.StatusOK},
		{method: http.MethodHead, path: "/v1.45/plugins", status: http.StatusOK},
		{method: http.MethodPost, path: "/v1.45/plugins/create", status: http.StatusNotImplemented},
		{method: http.MethodGet, path: "/v1.45/plugins/privileges?remote=docker.io/library/example:latest", status: http.StatusOK},
		{method: http.MethodHead, path: "/v1.45/plugins/privileges?remote=docker.io/library/example:latest", status: http.StatusOK},
		{method: http.MethodGet, path: "/v1.45/plugins/example/json", status: http.StatusNotFound},
		{method: http.MethodHead, path: "/v1.45/plugins/example/json", status: http.StatusNotFound},
		{method: http.MethodGet, path: "/v1.45/plugins/example/yaml", status: http.StatusNotFound},
		{method: http.MethodPost, path: "/v1.45/plugins/pull?remote=docker.io/library/example:latest", status: http.StatusNotImplemented},
		{method: http.MethodPost, path: "/v1.45/plugins/example/enable", status: http.StatusNotFound},
		{method: http.MethodPost, path: "/v1.45/plugins/example/disable", status: http.StatusNotFound},
		{method: http.MethodPost, path: "/v1.45/plugins/example/push", status: http.StatusNotImplemented},
		{method: http.MethodPost, path: "/v1.45/plugins/example/set", status: http.StatusNotImplemented},
		{method: http.MethodPost, path: "/v1.45/plugins/example/upgrade?remote=docker.io/library/example:latest", status: http.StatusNotImplemented},
		{method: http.MethodDelete, path: "/v1.45/plugins/example", status: http.StatusNotFound},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			t.Parallel()
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
			if rr.Code != tc.status {
				t.Fatalf("expected %d, got %d body=%s", tc.status, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestCreateVolumeAcceptsEmptyBodyAndNormalizesMaps(t *testing.T) {
	t.Parallel()

	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store init: %v", err)
	}
	h := &Handler{
		store:      st,
		attachPTYs: map[string]*os.File{},
		events:     newEventBroker(),
	}

	createRR := httptest.NewRecorder()
	h.createVolume(createRR, httptest.NewRequest(http.MethodPost, "/v1.45/volumes/create", nil))
	if createRR.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", createRR.Code, createRR.Body.String())
	}

	var created VolumeCreateResponse
	if err := json.NewDecoder(createRR.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Name == "" {
		t.Fatal("expected auto-generated volume name")
	}
	if created.Labels == nil || created.Options == nil {
		t.Fatalf("expected normalized maps, got labels=%#v options=%#v", created.Labels, created.Options)
	}

	inspectReq := httptest.NewRequest(http.MethodGet, "/v1.45/volumes/"+created.Name, nil)
	inspectReq = mux.SetURLVars(inspectReq, map[string]string{"name": created.Name})
	inspectRR := httptest.NewRecorder()
	h.inspectVolume(inspectRR, inspectReq)
	if inspectRR.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", inspectRR.Code, inspectRR.Body.String())
	}
	var usage VolumeUsage
	if err := json.NewDecoder(inspectRR.Body).Decode(&usage); err != nil {
		t.Fatalf("decode inspect response: %v", err)
	}
	if usage.Labels == nil || usage.Options == nil {
		t.Fatalf("expected normalized inspect maps, got labels=%#v options=%#v", usage.Labels, usage.Options)
	}
}
