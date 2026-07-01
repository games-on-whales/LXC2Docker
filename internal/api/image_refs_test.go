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

// TestImageRefsForIDDedupesDigests: `docker tag nginx:latest nginx:stable`
// copies the record (same repo + same RepoDigest), which would otherwise emit
// the identical repo@digest twice. Docker reports RepoDigests as a set.
func TestImageRefsForIDDedupesDigests(t *testing.T) {
	t.Parallel()
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	for _, ref := range []string{"nginx:latest", "nginx:stable"} {
		if err := st.AddImage(&store.ImageRecord{ID: "oci_nginx", Ref: ref, RepoDigest: "sha256:cafe"}); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{store: st}
	tags, digests := h.imageRefsForID("oci_nginx")
	if !reflect.DeepEqual(tags, []string{"nginx:latest", "nginx:stable"}) {
		t.Errorf("RepoTags = %v", tags)
	}
	// Both refs are repo "nginx" @ same digest → one entry, not two.
	if !reflect.DeepEqual(digests, []string{"nginx@sha256:cafe"}) {
		t.Errorf("RepoDigests = %v, want single deduped entry", digests)
	}
}

// TestDigestRefsEmptyRepo: a dangling/untagged record (empty ref) with a
// RepoDigest must not produce a malformed "@sha256:..." entry.
func TestDigestRefsEmptyRepo(t *testing.T) {
	t.Parallel()
	got := digestRefs(&store.ImageRecord{Ref: "", RepoDigest: "sha256:cafe"})
	if got == nil || len(got) != 0 {
		t.Errorf("digestRefs(empty ref) = %v, want empty non-nil slice", got)
	}
	// A real repo still produces the normal shape.
	got = digestRefs(&store.ImageRecord{Ref: "nginx:latest", RepoDigest: "sha256:cafe"})
	if len(got) != 1 || got[0] != "nginx@sha256:cafe" {
		t.Errorf("digestRefs(nginx) = %v, want [nginx@sha256:cafe]", got)
	}
}

func TestSortedUnique(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   []string
		want []string
	}{
		{nil, []string{}},
		{[]string{}, []string{}},
		{[]string{"b", "a", "b", "c", "a"}, []string{"a", "b", "c"}},
		{[]string{"x"}, []string{"x"}},
	}
	for _, tc := range cases {
		got := sortedUnique(tc.in)
		if got == nil {
			t.Errorf("sortedUnique(%v) returned nil, want non-nil", tc.in)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("sortedUnique(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
