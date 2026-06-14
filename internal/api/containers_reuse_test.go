package api

import (
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestReuseContainersEnabled(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"off":   false,
		"1":     true,
		"true":  true,
		"TRUE":  true,
		"Yes":   true,
		"on":    true,
	}
	for val, want := range cases {
		t.Setenv("DLD_REUSE_CONTAINERS", val)
		if got := reuseContainersEnabled(); got != want {
			t.Errorf("DLD_REUSE_CONTAINERS=%q: got %v, want %v", val, got, want)
		}
	}
}

// TestReusableContainerGuards covers the guard paths that decide reuse WITHOUT
// touching the manager (the running-state check is only reached once these pass,
// so these can run with a nil mgr).
func TestReusableContainerGuards(t *testing.T) {
	h := &Handler{} // mgr deliberately nil: no guard below reaches State()

	old := &store.ContainerRecord{ID: "abc", Name: "steam_42", Image: "img", ImageID: "img:tag", VMID: 100}

	// Reuse disabled → never reusable, even on an otherwise-perfect match.
	t.Setenv("DLD_REUSE_CONTAINERS", "0")
	if h.reusableContainer(old, "img:tag") {
		t.Error("reuse disabled: expected not reusable")
	}

	t.Setenv("DLD_REUSE_CONTAINERS", "1")

	// VMID 0 (non-PVE-CT path) is not adoptable.
	noVMID := *old
	noVMID.VMID = 0
	if h.reusableContainer(&noVMID, "img:tag") {
		t.Error("VMID 0: expected not reusable")
	}

	// Different image → warm rootfs is stale, must clone fresh.
	if h.reusableContainer(old, "other:tag") {
		t.Error("image mismatch: expected not reusable")
	}

	// nil record is never reusable.
	if h.reusableContainer(nil, "img:tag") {
		t.Error("nil record: expected not reusable")
	}
}
