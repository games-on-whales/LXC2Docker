package api

import (
	"os/exec"
	"testing"
	"time"
)

func TestStartDetachedExecMarksExecRunningForPortainer(t *testing.T) {
	t.Parallel()

	h := &Handler{execs: newExecStore()}
	rec := &execRecord{ID: "exec1"}
	h.execs.add(rec)

	cmd := exec.Command("sh", "-c", "sleep 0.1")
	h.startDetachedExec(rec.ID, cmd)

	started := h.execs.get(rec.ID)
	if started == nil {
		t.Fatal("expected exec record")
	}
	if !started.Running {
		t.Fatal("expected detached exec to be marked running immediately")
	}
	if started.StartedAt.IsZero() {
		t.Fatal("expected detached exec to record StartedAt")
	}

	time.Sleep(200 * time.Millisecond)
	finished := h.execs.get(rec.ID)
	if finished == nil {
		t.Fatal("expected exec record after completion")
	}
	if finished.Running {
		t.Fatal("expected detached exec to clear running after completion")
	}
	if finished.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", finished.ExitCode)
	}
}
