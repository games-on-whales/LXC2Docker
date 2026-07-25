package lxc

import (
	"testing"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func newStampManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	return &Manager{lxcPath: t.TempDir(), store: st}, st
}

const stampID = "cccccccccccc0000000000000000000000000000000000000000000000000000"

// StartedAt used to be stamped only by the API /start handler, so a container
// brought up by the restart-policy watcher or the startup reconcile kept a nil
// StartedAt: `docker inspect` reported the Go zero time for a running
// container, and the watcher went on treating it as "never started" — which
// skips restart-policy handling entirely. StartContainer is the choke point all
// start paths share, so the stamp belongs there.
func TestStampStartedSetsStartedAt(t *testing.T) {
	mgr, st := newStampManager(t)
	if err := st.AddContainer(&store.ContainerRecord{
		ID:            stampID,
		Name:          "never-started",
		RestartPolicy: "always",
	}); err != nil {
		t.Fatalf("AddContainer: %v", err)
	}

	before := time.Now()
	mgr.stampStarted(stampID)

	rec := st.GetContainer(stampID)
	if rec == nil {
		t.Fatal("container vanished from store")
	}
	if rec.StartedAt == nil {
		t.Fatal("StartedAt is nil after start; inspect would report 0001-01-01T00:00:00Z")
	}
	if rec.StartedAt.Before(before) {
		t.Errorf("StartedAt = %v, want >= %v", rec.StartedAt, before)
	}
}

// Docker refreshes StartedAt on every start, not just the first, and clears the
// prior run's exit time once the container is running again.
func TestStampStartedRefreshesOnRestart(t *testing.T) {
	mgr, st := newStampManager(t)
	old := time.Now().Add(-time.Hour)
	if err := st.AddContainer(&store.ContainerRecord{
		ID:         stampID,
		Name:       "restarted",
		StartedAt:  &old,
		FinishedAt: &old,
	}); err != nil {
		t.Fatalf("AddContainer: %v", err)
	}

	mgr.stampStarted(stampID)

	rec := st.GetContainer(stampID)
	if rec.StartedAt == nil || !rec.StartedAt.After(old) {
		t.Errorf("StartedAt = %v, want refreshed past %v", rec.StartedAt, old)
	}
	if rec.FinishedAt != nil {
		t.Errorf("FinishedAt = %v, want nil while running", rec.FinishedAt)
	}
}

// The stamp runs on every start path, including ones where the store record was
// removed concurrently (e.g. an --rm container reaped mid-start). It must not panic.
func TestStampStartedMissingRecordIsNoop(t *testing.T) {
	mgr, _ := newStampManager(t)
	mgr.stampStarted("dddddddddddd0000000000000000000000000000000000000000000000000000")
}
