package api

import (
	"os/exec"
	"testing"
	"time"
)

func TestDetachedExecTracksPidForPortainerInspect(t *testing.T) {
	t.Parallel()

	h := &Handler{execs: newExecStore()}
	rec := &execRecord{ID: "exec-pid"}
	h.execs.add(rec)

	cmd := exec.Command("sh", "-c", "sleep 0.1")
	h.startDetachedExec(rec.ID, cmd)

	time.Sleep(20 * time.Millisecond)
	running := h.execs.get(rec.ID)
	if running == nil {
		t.Fatal("expected exec record")
	}
	if !running.Running {
		t.Fatal("expected detached exec to be running")
	}
	if running.Pid <= 0 {
		t.Fatalf("expected detached exec to have a pid, got %d", running.Pid)
	}

	time.Sleep(200 * time.Millisecond)
	finished := h.execs.get(rec.ID)
	if finished == nil {
		t.Fatal("expected exec record after completion")
	}
	if finished.Running {
		t.Fatal("expected detached exec to stop running")
	}
	if finished.Pid != 0 {
		t.Fatalf("expected finished exec pid to clear, got %d", finished.Pid)
	}
}
