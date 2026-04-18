package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestSyntheticImageSearchNameSupportsPortainerPullSearch(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"ubuntu":                              "ubuntu",
		"docker.io/library/nginx:latest":      "nginx",
		"ghcr.io/example/team/app:1.2.3":      "example/team/app",
		"registry.example.com/ns/app@sha256:": "ns/app",
	}
	for in, want := range tests {
		if got := syntheticImageSearchName(in); got != want {
			t.Fatalf("syntheticImageSearchName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchImagesSynthesizesPullableResultWhenNoLocalMatch(t *testing.T) {
	t.Parallel()

	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	h := &Handler{store: st}

	req := httptest.NewRequest(http.MethodGet, "/v1.45/images/search?term=docker.io/library/redis:latest", nil)
	rr := httptest.NewRecorder()
	h.searchImages(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var out []ImageSearchResult
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 synthetic result, got %#v", out)
	}
	if out[0].Name != "redis" {
		t.Fatalf("expected synthetic redis result, got %#v", out[0])
	}
}
