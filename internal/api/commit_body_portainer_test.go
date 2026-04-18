package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestCommitContainerAppliesPortainerBodyConfigAndChanges(t *testing.T) {
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
		OCIShell:   []string{"/bin/sh", "-c"},
	}
	if err := st.AddImage(src); err != nil {
		t.Fatalf("add image: %v", err)
	}
	if err := st.AddContainer(&store.ContainerRecord{
		ID:      "bodycfg",
		Name:    "bodycfg",
		Image:   src.Ref,
		ImageID: src.Ref,
		Created: time.Unix(20, 0),
	}); err != nil {
		t.Fatalf("add container: %v", err)
	}

	user := "1000:1000"
	workdir := "/workspace"
	stopSignal := "SIGQUIT"
	body := commitConfig{
		Cmd:          []string{"serve"},
		Entrypoint:   []string{"/entry"},
		Env:          []string{"A=B"},
		Labels:       map[string]string{"app": "demo"},
		User:         &user,
		WorkingDir:   &workdir,
		StopSignal:   &stopSignal,
		ExposedPorts: map[string]struct{}{"8080/tcp": {}},
		Volumes:      map[string]struct{}{"/data": {}},
		Healthcheck:  &Healthcheck{Test: []string{"CMD-SHELL", "echo ok"}, Interval: int64(30 * time.Second)},
		Shell:        []string{"/bin/bash", "-c"},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	h := &Handler{store: st}
	req := httptest.NewRequest(http.MethodPost, "/v1.45/commit?container=bodycfg&repo=example/bodycfg&tag=latest&changes=ENV+X=Y&changes=EXPOSE+9090/tcp", bytes.NewReader(payload))
	rr := httptest.NewRecorder()
	h.commitContainer(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}

	committed := st.GetImage(normalizeImageRef("example/bodycfg:latest"))
	if committed == nil {
		t.Fatal("expected committed image record")
	}
	if len(committed.OCIEntrypoint) != 1 || committed.OCIEntrypoint[0] != "/entry" {
		t.Fatalf("expected body entrypoint, got %#v", committed.OCIEntrypoint)
	}
	if len(committed.OCICmd) != 1 || committed.OCICmd[0] != "serve" {
		t.Fatalf("expected body cmd, got %#v", committed.OCICmd)
	}
	if len(committed.OCIEnv) != 2 {
		t.Fatalf("expected body env plus change env, got %#v", committed.OCIEnv)
	}
	if committed.OCILabels["app"] != "demo" {
		t.Fatalf("expected body labels, got %#v", committed.OCILabels)
	}
	if committed.OCIUser != "1000:1000" || committed.OCIWorkingDir != "/workspace" || committed.OCIStopSignal != "SIGQUIT" {
		t.Fatalf("expected body overrides, got %+v", committed)
	}
	if len(committed.OCIPorts) != 2 {
		t.Fatalf("expected body ports plus change port, got %#v", committed.OCIPorts)
	}
	if len(committed.OCIVolumes) != 1 || committed.OCIVolumes[0] != "/data" {
		t.Fatalf("expected body volumes, got %#v", committed.OCIVolumes)
	}
	if committed.OCIHealthcheck == nil || len(committed.OCIHealthcheck.Test) != 2 || committed.OCIHealthcheck.Test[1] != "echo ok" {
		t.Fatalf("expected body healthcheck, got %#v", committed.OCIHealthcheck)
	}
	if len(committed.OCIShell) != 2 || committed.OCIShell[0] != "/bin/bash" {
		t.Fatalf("expected body shell, got %#v", committed.OCIShell)
	}
}

func TestCommitContainerAcceptsEmptyBodyForPortainer(t *testing.T) {
	t.Parallel()

	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	src := &store.ImageRecord{
		ID:      "img2",
		Ref:     normalizeImageRef("docker.io/library/base:latest"),
		Created: time.Unix(10, 0),
	}
	if err := st.AddImage(src); err != nil {
		t.Fatalf("add image: %v", err)
	}
	if err := st.AddContainer(&store.ContainerRecord{
		ID:      "emptybody",
		Name:    "emptybody",
		Image:   src.Ref,
		ImageID: src.Ref,
		Created: time.Unix(20, 0),
	}); err != nil {
		t.Fatalf("add container: %v", err)
	}

	h := &Handler{store: st}
	req := httptest.NewRequest(http.MethodPost, "/v1.45/commit?container=emptybody&repo=example/emptybody&tag=latest", bytes.NewReader(nil))
	rr := httptest.NewRecorder()
	h.commitContainer(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 for empty body, got %d body=%s", rr.Code, rr.Body.String())
	}
}
