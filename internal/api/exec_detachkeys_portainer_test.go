package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
)

func TestExecInspectPreservesDetachKeysForPortainer(t *testing.T) {
	t.Parallel()

	h := &Handler{execs: newExecStore()}
	h.execs.add(&execRecord{
		ID:           "exec-detach",
		ContainerID:  "abc123",
		Cmd:          []string{"sh"},
		Tty:          true,
		DetachKeys:   "ctrl-p,ctrl-q",
		AttachStdin:  true,
		AttachStdout: true,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1.45/exec/exec-detach/json", nil)
	req = mux.SetURLVars(req, map[string]string{"id": "exec-detach"})
	rr := httptest.NewRecorder()
	h.execInspect(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var out ExecInspect
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.DetachKeys != "ctrl-p,ctrl-q" {
		t.Fatalf("expected detach keys to round-trip, got %q", out.DetachKeys)
	}
}
