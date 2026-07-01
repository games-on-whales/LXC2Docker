package api

import (
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestImageDisplayID(t *testing.T) {
	t.Parallel()
	// Real config digest wins.
	if got := imageDisplayID(&store.ImageRecord{ID: "commit_abc", ConfigDigest: "deadbeef"}); got != "deadbeef" {
		t.Errorf("with ConfigDigest = %q, want deadbeef", got)
	}
	// Legacy record (no digest) falls back to the internal ID.
	if got := imageDisplayID(&store.ImageRecord{ID: "oci_nginx"}); got != "oci_nginx" {
		t.Errorf("legacy = %q, want oci_nginx", got)
	}
}

func TestFindImageByIDMatchesConfigDigest(t *testing.T) {
	t.Parallel()
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	full := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if err := st.AddImage(&store.ImageRecord{ID: "commit_x", Ref: "app:1", ConfigDigest: full}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddImage(&store.ImageRecord{ID: "oci_nginx", Ref: "nginx:latest"}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: st}

	// Resolve by full config digest, with and without the sha256: prefix.
	if got := h.findImageByID("sha256:" + full); got == nil || got.Ref != "app:1" {
		t.Errorf("by full digest (sha256:) did not resolve to app:1: %+v", got)
	}
	if got := h.findImageByID(full); got == nil || got.Ref != "app:1" {
		t.Errorf("by full digest did not resolve to app:1: %+v", got)
	}
	// Resolve by a short (>=4 char) digest prefix.
	if got := h.findImageByID(full[:12]); got == nil || got.Ref != "app:1" {
		t.Errorf("by digest prefix did not resolve: %+v", got)
	}
	// Still resolves legacy records by internal ID.
	if got := h.findImageByID("oci_nginx"); got == nil || got.Ref != "nginx:latest" {
		t.Errorf("by internal ID did not resolve: %+v", got)
	}
	// Unknown → nil.
	if got := h.findImageByID("ffff"); got != nil {
		t.Errorf("unknown short id resolved to %+v", got)
	}
}
