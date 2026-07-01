package api

import (
	"testing"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestFirstFilterValue(t *testing.T) {
	t.Parallel()
	if got := firstFilterValue(nil); got != "" {
		t.Errorf("nil = %q, want empty", got)
	}
	if got := firstFilterValue([]string{"", "a", "b"}); got != "a" {
		t.Errorf("skip-empty = %q, want a", got)
	}
}

func TestImageCreatedByRef(t *testing.T) {
	t.Parallel()
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := st.AddImage(&store.ImageRecord{ID: "oci_nginx", Ref: "nginx:latest", Created: created}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: st}

	// Resolve by ref.
	if got, ok := h.imageCreatedByRef("nginx:latest"); !ok || !got.Equal(created) {
		t.Errorf("by ref = (%v,%v), want (%v,true)", got, ok, created)
	}
	// Resolve by ID.
	if got, ok := h.imageCreatedByRef("oci_nginx"); !ok || !got.Equal(created) {
		t.Errorf("by id = (%v,%v), want (%v,true)", got, ok, created)
	}
	// Unknown → false (handler turns this into 404).
	if _, ok := h.imageCreatedByRef("ghost:1"); ok {
		t.Errorf("unknown ref should not resolve")
	}
}
