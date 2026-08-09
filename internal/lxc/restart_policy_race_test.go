package lxc

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestRestartWatcherWaitsForInFlightStopIntent(t *testing.T) {
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	id := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	startedAt := time.Now().Add(-time.Minute)
	if err := st.AddContainer(&store.ContainerRecord{
		ID:            id,
		Name:          "wolf",
		StartedAt:     &startedAt,
		RestartPolicy: "unless-stopped",
	}); err != nil {
		t.Fatalf("AddContainer: %v", err)
	}

	binDir := t.TempDir()
	startMarker := filepath.Join(t.TempDir(), "lxc-start-called")
	if err := os.WriteFile(filepath.Join(binDir, "lxc-info"), []byte("#!/bin/sh\necho STOPPED\n"), 0o755); err != nil {
		t.Fatalf("write fake lxc-info: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "lxc-start"), []byte("#!/bin/sh\ntouch \"$START_MARKER\"\n"), 0o755); err != nil {
		t.Fatalf("write fake lxc-start: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("START_MARKER", startMarker)

	// Model a stop request that owns the lifecycle operation and has persisted
	// intent, but has not yet finished stopping the workload.
	unlock := st.LockContainerLifecycle(id)
	if _, err := st.UpdateContainer(id, func(rec *store.ContainerRecord) {
		rec.StoppedByUser = true
	}); err != nil {
		t.Fatalf("persist stop intent: %v", err)
	}

	mgr := &Manager{lxcPath: t.TempDir(), store: st}
	watcherStarted := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		close(watcherStarted)
		mgr.enforceRestartPolicy(id, nil)
		close(watcherDone)
	}()
	<-watcherStarted
	select {
	case <-watcherDone:
		t.Fatal("restart watcher did not wait for the in-flight stop")
	case <-time.After(50 * time.Millisecond):
	}

	unlock()
	select {
	case <-watcherDone:
	case <-time.After(time.Second):
		t.Fatal("restart watcher did not resume after the stop released its guard")
	}
	if _, err := os.Stat(startMarker); !os.IsNotExist(err) {
		t.Fatalf("restart watcher started a container with persisted stop intent: %v", err)
	}
}
