package api

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestExecStoreRecordsAreIsolated(t *testing.T) {
	t.Parallel()

	s := newExecStore()
	original := &execRecord{
		ID:  "isolated",
		Cmd: []string{"original"},
		Env: []string{"OWNER=store"},
	}
	s.add(original)
	original.Cmd[0] = "input-alias"

	got := s.get(original.ID)
	got.Running = true
	got.Cmd[0] = "get-alias"
	got.Env[0] = "OWNER=caller"

	stored := s.get(original.ID)
	if stored.Running {
		t.Fatal("mutation through get changed stored Running")
	}
	if stored.Cmd[0] != "original" {
		t.Fatalf("stored Cmd = %q, want original", stored.Cmd[0])
	}
	if stored.Env[0] != "OWNER=store" {
		t.Fatalf("stored Env = %q, want OWNER=store", stored.Env[0])
	}
}

func TestExecStoreConcurrentReadsAreIsolated(t *testing.T) {
	t.Parallel()

	s := newExecStore()
	s.add(&execRecord{ID: "read-race"})
	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				rec := s.get("read-race")
				rec.Running = !rec.Running
				rec.ExitCode++
			}
		}()
	}
	wg.Wait()
}

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
