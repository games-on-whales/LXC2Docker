package api

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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

	err := h.stopWithIntent(rec.ID, func() error {
		if _, err := h.store.UpdateContainer(rec.ID, func(current *store.ContainerRecord) {
			current.StartError = "concurrent update"
		}); err != nil {
			t.Fatalf("concurrent UpdateContainer: %v", err)
		}
		return errStopFailed
	})
	if err != errStopFailed {
		t.Fatalf("stopWithIntent err = %v, want %v", err, errStopFailed)
	}
	if h.store.GetContainer(rec.ID).StoppedByUser {
		t.Fatal("a failed stop left the container marked stopped-by-user, " +
			"which disables its restart policy")
	}
	if got := h.store.GetContainer(rec.ID).StartError; got != "concurrent update" {
		t.Fatalf("failed-stop restore overwrote %q, want concurrent update", got)
	}
}

func TestStopWithIntentSerializesOverlappingStops(t *testing.T) {
	h, rec := stopIntentFixture(t)

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstResult := make(chan error, 1)
	go func() {
		firstResult <- h.stopWithIntent(rec.ID, func() error {
			close(firstEntered)
			<-releaseFirst
			return errStopFailed
		})
	}()
	<-firstEntered

	secondEntered := make(chan struct{})
	secondResult := make(chan error, 1)
	go func() {
		secondResult <- h.stopWithIntent(rec.ID, func() error {
			close(secondEntered)
			return nil
		})
	}()

	select {
	case <-secondEntered:
		t.Fatal("a second stop ran while the first still owned the lifecycle operation")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstResult; err != errStopFailed {
		t.Fatalf("first stop err = %v, want %v", err, errStopFailed)
	}
	if err := <-secondResult; err != nil {
		t.Fatalf("second stop failed: %v", err)
	}
	if !h.store.GetContainer(rec.ID).StoppedByUser {
		t.Fatal("the failed first stop rolled back the successful second stop's intent")
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

func TestStopWithIntentDoesNotStopWhenIntentCannotPersist(t *testing.T) {
	base := t.TempDir()
	st, err := store.NewAt(base)
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	rec := &store.ContainerRecord{ID: "persist-failure", Name: "wolf"}
	if err := st.AddContainer(rec); err != nil {
		t.Fatalf("AddContainer: %v", err)
	}
	if err := os.RemoveAll(base); err != nil {
		t.Fatalf("remove store directory: %v", err)
	}

	ran := false
	err = (&Handler{store: st}).stopWithIntent(rec.ID, func() error {
		ran = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "persist stop intent") {
		t.Fatalf("stopWithIntent err = %v, want persistence failure", err)
	}
	if ran {
		t.Fatal("stop callback ran without durable stop intent")
	}
}

func TestStopWithIntentSurfacesRollbackPersistenceFailure(t *testing.T) {
	base := t.TempDir()
	st, err := store.NewAt(base)
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	rec := &store.ContainerRecord{ID: "rollback-failure", Name: "wolf"}
	if err := st.AddContainer(rec); err != nil {
		t.Fatalf("AddContainer: %v", err)
	}

	err = (&Handler{store: st}).stopWithIntent(rec.ID, func() error {
		if err := os.RemoveAll(base); err != nil {
			t.Fatalf("remove store directory: %v", err)
		}
		return errStopFailed
	})
	if !errors.Is(err, errStopFailed) || !strings.Contains(err.Error(), "restore stop intent") {
		t.Fatalf("stopWithIntent err = %v, want stop and rollback failures", err)
	}
}

var errStopFailed = errStop("stop failed")

type errStop string

func (e errStop) Error() string { return string(e) }
