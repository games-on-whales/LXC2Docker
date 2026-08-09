package api

import (
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// A container that is meant to be running: unless-stopped, started, and not
// stopped by anyone yet.
func stopIntentFixture(t *testing.T) (*Handler, *store.ContainerRecord) {
	t.Helper()
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	rec := &store.ContainerRecord{
		ID:            "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Name:          "wolf",
		RestartPolicy: "unless-stopped",
	}
	if err := st.AddContainer(rec); err != nil {
		t.Fatalf("AddContainer: %v", err)
	}
	return &Handler{store: st}, rec
}

// The bug this pins: intent used to be recorded *after* the stop returned, and
// the stop returns only once init is dead. The restart watcher polls
// independently, so any tick landing between the container's exit and that
// write saw an exited container with StoppedByUser unset — and restarted it.
// From the outside, `docker stop wolf` did nothing at all.
//
// The stop callback stands in for that window: it runs while the container is
// going down, which is exactly when the watcher can look.
func TestStopWithIntentRecordsIntentBeforeStopping(t *testing.T) {
	h, rec := stopIntentFixture(t)

	seenDuringStop := false
	err := h.stopWithIntent(rec.ID, func() error {
		seenDuringStop = h.store.GetContainer(rec.ID).StoppedByUser
		return nil
	})
	if err != nil {
		t.Fatalf("stopWithIntent: %v", err)
	}
	if !seenDuringStop {
		t.Fatal("StoppedByUser was not set while the container was being stopped — " +
			"a restart-watcher tick in that window would restart it")
	}
	if !h.store.GetContainer(rec.ID).StoppedByUser {
		t.Fatal("StoppedByUser not set after the stop")
	}
}

// A stop that failed did not stop anything, so the container is still running
// and must keep its restart policy. Marking it stopped-by-user anyway would
// silently disable the policy for the next real exit.
func TestStopWithIntentRestoresIntentWhenTheStopFails(t *testing.T) {
	h, rec := stopIntentFixture(t)

	err := h.stopWithIntent(rec.ID, func() error { return errStopFailed })
	if err != errStopFailed {
		t.Fatalf("stopWithIntent err = %v, want %v", err, errStopFailed)
	}
	if h.store.GetContainer(rec.ID).StoppedByUser {
		t.Fatal("a failed stop left the container marked stopped-by-user, " +
			"which disables its restart policy")
	}
}

// A container the user had already stopped stays stopped when a later stop
// fails — the restore must put back the previous value, not clear it.
func TestStopWithIntentKeepsAnEarlierStopOnFailure(t *testing.T) {
	h, rec := stopIntentFixture(t)
	rec.StoppedByUser = true
	if err := h.store.AddContainer(rec); err != nil {
		t.Fatalf("AddContainer: %v", err)
	}

	_ = h.stopWithIntent(rec.ID, func() error { return errStopFailed })

	if !h.store.GetContainer(rec.ID).StoppedByUser {
		t.Fatal("a failed stop cleared an intent the user had already expressed")
	}
}

// An unknown id must not panic: the container may have been removed between the
// route resolving it and the stop running.
func TestStopWithIntentToleratesAMissingRecord(t *testing.T) {
	h, _ := stopIntentFixture(t)

	ran := false
	if err := h.stopWithIntent("nosuchcontainer", func() error { ran = true; return nil }); err != nil {
		t.Fatalf("stopWithIntent: %v", err)
	}
	if !ran {
		t.Fatal("the stop was skipped for a container with no store record")
	}
}

var errStopFailed = errStop("stop failed")

type errStop string

func (e errStop) Error() string { return string(e) }
