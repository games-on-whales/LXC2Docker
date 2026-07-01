package api

import "testing"

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
