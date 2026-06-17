package lxc

import (
	"testing"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// In a test environment lxc-info is unavailable, so Manager.State reports
// "exited" for every record. That mirrors the real daemon's view of a
// container that has been created but not yet started (lxc-info returns
// nothing for a not-yet-running / still-being-created CT). The GC must treat
// such a container as "created", not "exited", and leave it alone — otherwise
// it races a `docker run` between the create and start calls and deletes the
// container out from under the imminent attach (which then 404s). A slow image
// clone (e.g. a freshly built image) widens that window to seconds.
func TestGC_SkipsNeverStartedEphemeralContainer(t *testing.T) {
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	mgr := &Manager{lxcPath: t.TempDir(), store: st}

	// Created but never started: StartedAt == nil. This is the racing
	// `docker run` case the GC must not touch.
	created := &store.ContainerRecord{
		ID:         "aaaaaaaaaaaa0000000000000000000000000000000000000000000000000000",
		Name:       "created-never-started",
		AutoRemove: true,
		Created:    time.Now(),
	}
	// Started and since exited: StartedAt set. This is a real `--rm` leftover
	// the GC is supposed to reap.
	now := time.Now()
	exited := &store.ContainerRecord{
		ID:         "bbbbbbbbbbbb0000000000000000000000000000000000000000000000000000",
		Name:       "started-then-exited",
		AutoRemove: true,
		Created:    now,
		StartedAt:  &now,
	}
	if err := st.AddContainer(created); err != nil {
		t.Fatalf("AddContainer(created): %v", err)
	}
	if err := st.AddContainer(exited); err != nil {
		t.Fatalf("AddContainer(exited): %v", err)
	}

	mgr.gc()

	if st.GetContainer(created.ID) == nil {
		t.Errorf("GC reaped a created-but-never-started container; it must be left for the in-flight `docker run`")
	}
	if st.GetContainer(exited.ID) != nil {
		t.Errorf("GC did not reap a started-then-exited ephemeral container")
	}
}
