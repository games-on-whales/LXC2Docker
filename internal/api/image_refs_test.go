package api

import (
	"reflect"
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// TestImageRefsForID: inspect must report every tag pointing at an image ID,
// not just the queried ref — `docker tag` copies the record keeping the ID.
func TestImageRefsForID(t *testing.T) {
	t.Parallel()
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	// Two refs sharing one ID (as `docker tag` produces), plus a digest on one,
	// and an unrelated image with a different ID.
	if err := st.AddImage(&store.ImageRecord{ID: "oci_nginx_latest", Ref: "nginx:latest", RepoDigest: "sha256:deadbeef"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddImage(&store.ImageRecord{ID: "oci_nginx_latest", Ref: "myreg/nginx:pinned"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddImage(&store.ImageRecord{ID: "ubuntu_jammy", Ref: "ubuntu:22.04"}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: st}

	tags, digests := h.imageRefsForID("oci_nginx_latest")
	wantTags := []string{"myreg/nginx:pinned", "nginx:latest"} // sorted
	if !reflect.DeepEqual(tags, wantTags) {
		t.Errorf("RepoTags = %v, want %v", tags, wantTags)
	}
	wantDigests := []string{"nginx@sha256:deadbeef"}
	if !reflect.DeepEqual(digests, wantDigests) {
		t.Errorf("RepoDigests = %v, want %v", digests, wantDigests)
	}

	// The unrelated image must not leak into the group.
	otherTags, _ := h.imageRefsForID("ubuntu_jammy")
	if !reflect.DeepEqual(otherTags, []string{"ubuntu:22.04"}) {
		t.Errorf("ubuntu RepoTags = %v, want [ubuntu:22.04]", otherTags)
	}

	// Unknown ID yields non-nil empty slices (JSON [] not null).
	noneTags, noneDigests := h.imageRefsForID("does-not-exist")
	if noneTags == nil || len(noneTags) != 0 || noneDigests == nil || len(noneDigests) != 0 {
		t.Errorf("unknown ID = (%v,%v), want empty non-nil slices", noneTags, noneDigests)
	}
}
