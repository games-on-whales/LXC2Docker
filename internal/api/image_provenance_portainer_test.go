package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/lxc"
	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestImageInspectExposesPortainerProvenanceMetadata(t *testing.T) {
	t.Parallel()

	rec := &store.ImageRecord{
		ID:               "img1",
		Ref:              normalizeImageRef("docker.io/library/base:latest"),
		Created:          time.Unix(10, 0),
		Arch:             "amd64",
		OCIAuthor:        "Portainer",
		OCIComment:       "snapshot",
		OCIContainer:     "abc123",
		OCIDockerVersion: "26.0.0-portainer",
		OCIVariant:       "v8",
	}
	h := &Handler{store: mustStoreWithImage(t, rec), mgr: &lxc.Manager{}}

	req := httptest.NewRequest(http.MethodGet, "/v1.45/images/base/json", nil)
	rr := httptest.NewRecorder()
	h.inspectImage(rr, req.WithContext(req.Context()))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var out ImageInspect
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Author != "Portainer" || out.Comment != "snapshot" || out.Container != "abc123" || out.DockerVersion != "26.0.0-portainer" || out.Variant != "v8" {
		t.Fatalf("expected provenance metadata, got %+v", out)
	}
}

func TestCommitContainerPreservesPortainerProvenanceMetadata(t *testing.T) {
	t.Parallel()

	st := mustStoreWithImage(t, &store.ImageRecord{
		ID:               "img1",
		Ref:              normalizeImageRef("docker.io/library/base:latest"),
		Created:          time.Unix(10, 0),
		OCIAuthor:        "base-author",
		OCIComment:       "base-comment",
		OCIDockerVersion: "25.0.0-base",
		OCIVariant:       "v7",
	})
	if err := st.AddContainer(&store.ContainerRecord{
		ID:      "abc123",
		Name:    "demo",
		Image:   normalizeImageRef("docker.io/library/base:latest"),
		ImageID: normalizeImageRef("docker.io/library/base:latest"),
		Created: time.Unix(20, 0),
	}); err != nil {
		t.Fatalf("add container: %v", err)
	}

	h := &Handler{store: st}
	req := httptest.NewRequest(http.MethodPost, "/v1.45/commit?container=abc123&repo=example/provenance&tag=latest&author=Portainer&comment=snapshot", nil)
	rr := httptest.NewRecorder()
	h.commitContainer(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	committed := st.GetImage(normalizeImageRef("example/provenance:latest"))
	if committed == nil {
		t.Fatal("expected committed image")
	}
	if committed.OCIAuthor != "Portainer" || committed.OCIComment != "snapshot" || committed.OCIContainer != "abc123" || committed.OCIDockerVersion != "24.0.0-lxc" || committed.OCIVariant != "v7" {
		t.Fatalf("expected committed provenance metadata, got %+v", committed)
	}
}

func TestSaveLoadRoundTripsPortainerProvenanceMetadata(t *testing.T) {
	t.Parallel()

	cfgBytes, _, err := synthesiseImageConfig(&store.ImageRecord{
		Arch:             "amd64",
		Created:          time.Unix(10, 0),
		OCIAuthor:        "Portainer",
		OCIComment:       "snapshot",
		OCIContainer:     "abc123",
		OCIDockerVersion: "26.0.0-portainer",
		OCIVariant:       "v8",
	}, "layer")
	if err != nil {
		t.Fatalf("synthesise image config: %v", err)
	}
	var saved saveImageConfig
	if err := json.Unmarshal(cfgBytes, &saved); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if saved.Author != "Portainer" || saved.Comment != "snapshot" || saved.Container != "abc123" || saved.DockerVersion != "26.0.0-portainer" || saved.Variant != "v8" {
		t.Fatalf("expected saved provenance metadata, got %+v", saved)
	}
}

func mustStoreWithImage(t *testing.T, rec *store.ImageRecord) *store.Store {
	t.Helper()
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := st.AddImage(rec); err != nil {
		t.Fatalf("add image: %v", err)
	}
	return st
}
