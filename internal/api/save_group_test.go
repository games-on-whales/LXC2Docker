package api

import (
	"reflect"
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestGroupSaveRefs(t *testing.T) {
	t.Parallel()

	// Two tags of one image (same ID) collapse; a distinct image stays separate;
	// first-seen order preserved.
	recs := []*store.ImageRecord{
		{ID: "oci_nginx", Ref: "nginx:latest"},
		{ID: "oci_redis", Ref: "redis:7"},
		{ID: "oci_nginx", Ref: "myreg/nginx:pinned"},
	}
	got := groupSaveRefs(recs)
	want := [][]string{
		{"nginx:latest", "myreg/nginx:pinned"},
		{"redis:7"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("groupSaveRefs = %v, want %v", got, want)
	}

	// Representative is the first ref of each group.
	if got[0][0] != "nginx:latest" {
		t.Errorf("nginx representative = %q, want nginx:latest", got[0][0])
	}
}

func TestGroupSaveRefsIDLessFallback(t *testing.T) {
	t.Parallel()
	// Records with an empty ID must NOT be grouped together (each is its own
	// group, keyed by ref) — otherwise distinct images would share one entry.
	recs := []*store.ImageRecord{
		{ID: "", Ref: "a:1"},
		{ID: "", Ref: "b:2"},
	}
	got := groupSaveRefs(recs)
	want := [][]string{{"a:1"}, {"b:2"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("id-less grouping = %v, want %v", got, want)
	}
}
