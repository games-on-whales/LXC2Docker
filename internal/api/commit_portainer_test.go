package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestCommitContainerPreservesPortainerContainerOverrides(t *testing.T) {
	t.Parallel()

	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	src := &store.ImageRecord{
		ID:            "img1",
		Ref:           normalizeImageRef("docker.io/library/base:latest"),
		Created:       time.Unix(10, 0),
		OCIWorkingDir: "/base",
		OCIPorts:      []string{"80/tcp"},
		OCILabels:     map[string]string{"base": "1"},
		OCIUser:       "root",
		OCIStopSignal: "SIGTERM",
		OCIHealthcheck: &store.HealthcheckConfig{
			Test: []string{"CMD", "true"},
		},
		OCIVolumes: []string{"/base-data"},
	}
	if err := st.AddImage(src); err != nil {
		t.Fatalf("add image: %v", err)
	}
	rec := &store.ContainerRecord{
		ID:              "abc123",
		Name:            "demo",
		Image:           src.Ref,
		ImageID:         src.Ref,
		Created:         time.Unix(20, 0),
		Entrypoint:      []string{"/entry"},
		Cmd:             []string{"serve"},
		Env:             []string{"A=B"},
		WorkingDir:      "/work",
		User:            "1000:1000",
		Labels:          map[string]string{"app": "demo"},
		StopSignal:      "SIGQUIT",
		ExposedPorts:    map[string]struct{}{"8080/tcp": {}},
		Volumes:         map[string]struct{}{"/data": {}},
		HealthcheckTest: []string{"CMD-SHELL", "echo ok"},
	}
	if err := st.AddContainer(rec); err != nil {
		t.Fatalf("add container: %v", err)
	}

	h := &Handler{store: st}
	req := httptest.NewRequest(http.MethodPost, "/v1.45/commit?container=abc123&repo=example/committed&tag=latest", nil)
	rr := httptest.NewRecorder()
	h.commitContainer(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}

	committed := st.GetImage(normalizeImageRef("example/committed:latest"))
	if committed == nil {
		t.Fatal("expected committed image record")
	}
	if len(committed.OCIEntrypoint) != 1 || committed.OCIEntrypoint[0] != "/entry" {
		t.Fatalf("expected committed entrypoint, got %#v", committed.OCIEntrypoint)
	}
	if len(committed.OCICmd) != 1 || committed.OCICmd[0] != "serve" {
		t.Fatalf("expected committed cmd, got %#v", committed.OCICmd)
	}
	if len(committed.OCIEnv) != 1 || committed.OCIEnv[0] != "A=B" {
		t.Fatalf("expected committed env, got %#v", committed.OCIEnv)
	}
	if committed.OCIWorkingDir != "/work" || committed.OCIUser != "1000:1000" || committed.OCIStopSignal != "SIGQUIT" {
		t.Fatalf("expected committed overrides, got %+v", committed)
	}
	if len(committed.OCIPorts) != 1 || committed.OCIPorts[0] != "8080/tcp" {
		t.Fatalf("expected committed exposed ports, got %#v", committed.OCIPorts)
	}
	if len(committed.OCIVolumes) != 1 || committed.OCIVolumes[0] != "/data" {
		t.Fatalf("expected committed volumes, got %#v", committed.OCIVolumes)
	}
	if committed.OCILabels["app"] != "demo" {
		t.Fatalf("expected committed labels, got %#v", committed.OCILabels)
	}
	if committed.OCIHealthcheck == nil || len(committed.OCIHealthcheck.Test) != 2 || committed.OCIHealthcheck.Test[1] != "echo ok" {
		t.Fatalf("expected committed healthcheck, got %#v", committed.OCIHealthcheck)
	}
}

func TestCommitContainerFallsBackToSourceImageDefaults(t *testing.T) {
	t.Parallel()

	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	src := &store.ImageRecord{
		ID:            "img2",
		Ref:           normalizeImageRef("docker.io/library/base:latest"),
		Created:       time.Unix(10, 0),
		OCIWorkingDir: "/base",
		OCIPorts:      []string{"80/tcp"},
		OCILabels:     map[string]string{"base": "1"},
		OCIUser:       "root",
		OCIStopSignal: "SIGTERM",
		OCIHealthcheck: &store.HealthcheckConfig{
			Test: []string{"CMD", "true"},
		},
		OCIVolumes: []string{"/base-data"},
	}
	if err := st.AddImage(src); err != nil {
		t.Fatalf("add image: %v", err)
	}
	if err := st.AddContainer(&store.ContainerRecord{
		ID:      "fallback",
		Name:    "fallback",
		Image:   src.Ref,
		ImageID: src.Ref,
		Created: time.Unix(20, 0),
	}); err != nil {
		t.Fatalf("add container: %v", err)
	}

	h := &Handler{store: st}
	req := httptest.NewRequest(http.MethodPost, "/v1.45/commit?container=fallback&repo=example/fallback&tag=latest", nil)
	rr := httptest.NewRecorder()
	h.commitContainer(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}

	committed := st.GetImage(normalizeImageRef("example/fallback:latest"))
	if committed == nil {
		t.Fatal("expected committed image record")
	}
	if committed.OCIWorkingDir != "/base" || committed.OCIUser != "root" || committed.OCIStopSignal != "SIGTERM" {
		t.Fatalf("expected source defaults to survive, got %+v", committed)
	}
	if len(committed.OCIPorts) != 1 || committed.OCIPorts[0] != "80/tcp" {
		t.Fatalf("expected fallback ports, got %#v", committed.OCIPorts)
	}
	if len(committed.OCIVolumes) != 1 || committed.OCIVolumes[0] != "/base-data" {
		t.Fatalf("expected fallback volumes, got %#v", committed.OCIVolumes)
	}
	if committed.OCILabels["base"] != "1" {
		t.Fatalf("expected fallback labels, got %#v", committed.OCILabels)
	}
	if committed.OCIHealthcheck == nil || len(committed.OCIHealthcheck.Test) != 2 || committed.OCIHealthcheck.Test[1] != "true" {
		t.Fatalf("expected fallback healthcheck, got %#v", committed.OCIHealthcheck)
	}
}
