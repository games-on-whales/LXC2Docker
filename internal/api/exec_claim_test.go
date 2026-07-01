package api

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestExecClaimStartConcurrent: under a race of N simultaneous starts of the
// same exec, exactly one must win (the whole point of the atomic guard). Run
// with -race for the strongest signal.
func TestExecClaimStartConcurrent(t *testing.T) {
	t.Parallel()
	s := newExecStore()
	s.add(&execRecord{ID: "race"})
	const n = 64
	var wins int64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if s.claimStart("race") {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("concurrent claimStart winners = %d, want exactly 1", wins)
	}
}

// TestExecClaimStart: an exec instance may be started only once (Docker 409s a
// second start). claimStart is the atomic guard.
func TestExecClaimStart(t *testing.T) {
	t.Parallel()
	s := newExecStore()
	s.add(&execRecord{ID: "e1"})

	if !s.claimStart("e1") {
		t.Fatal("first claim should succeed")
	}
	if s.claimStart("e1") {
		t.Fatal("second claim should fail (already running)")
	}
	// A finished exec (Running=false but StartedAt set) still cannot restart.
	s.update("e1", func(r *execRecord) { r.Running = false })
	if s.claimStart("e1") {
		t.Fatal("finished exec (StartedAt set) should not be restartable")
	}
	// releaseStart undoes the claim so a retry can start.
	s.releaseStart("e1")
	if !s.claimStart("e1") {
		t.Fatal("after releaseStart, claim should succeed again")
	}
	// Unknown exec cannot be claimed.
	if s.claimStart("does-not-exist") {
		t.Fatal("claiming an unknown exec should fail")
	}
}
