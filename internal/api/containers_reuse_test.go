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

// TestReusableContainerDigestDrift covers the core fix: a mutable tag (":latest")
// can be repointed to a NEW digest under the SAME ref, so ref-match alone must not
// adopt a stale warm rootfs. These cases all return before the running-state check
// (so a nil mgr is fine) but need a real store for the image-digest lookup.
func TestReusableContainerDigestDrift(t *testing.T) {
	t.Setenv("DLD_REUSE_CONTAINERS", "1")
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	h := &Handler{store: st}

	// Current image backing :latest is at digest NEW.
	if err := st.AddImage(&store.ImageRecord{Ref: "img:latest", RepoDigest: "sha256:NEW"}); err != nil {
		t.Fatalf("AddImage: %v", err)
	}

	// Warm rootfs cloned from the OLD digest → must NOT be reused (the bug:
	// previously reused because the ref ":latest" still matched).
	old := &store.ContainerRecord{ID: "c1", Name: "n", ImageID: "img:latest", VMID: 100,
		ImageDigest: "sha256:OLD"}
	if h.reusableContainer(old, "img:latest") {
		t.Error("digest drift (OLD vs NEW): expected not reusable")
	}

	// Unknown clone-time digest (legacy record) can't be confirmed current → not reusable.
	legacy := &store.ContainerRecord{ID: "c2", Name: "n", ImageID: "img:latest", VMID: 100,
		ImageDigest: ""}
	if h.reusableContainer(legacy, "img:latest") {
		t.Error("unknown clone digest: expected not reusable")
	}

	// Current image digest unknown (image not recorded / no RepoDigest) → not reusable.
	missing := &store.ContainerRecord{ID: "c3", Name: "n", ImageID: "img:unknown", VMID: 100,
		ImageDigest: "sha256:OLD"}
	if h.reusableContainer(missing, "img:unknown") {
		t.Error("unknown current digest: expected not reusable")
	}
}
