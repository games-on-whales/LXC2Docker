package api

import (
	"testing"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestVolumeCreatedAt(t *testing.T) {
	t.Parallel()
	ta := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tb := time.Date(2025, 6, 7, 8, 9, 10, 0, time.UTC)

	// Named volumes historically set CreatedAt only.
	if got := volumeCreatedAt(&store.VolumeRecord{CreatedAt: ta}); !got.Equal(ta) {
		t.Errorf("CreatedAt-only = %v, want %v", got, ta)
	}
	// Anonymous volumes historically set Created only.
	if got := volumeCreatedAt(&store.VolumeRecord{Created: tb}); !got.Equal(tb) {
		t.Errorf("Created-only = %v, want %v", got, tb)
	}
	// Both set → prefer CreatedAt.
	if got := volumeCreatedAt(&store.VolumeRecord{Created: tb, CreatedAt: ta}); !got.Equal(ta) {
		t.Errorf("both set = %v, want CreatedAt %v", got, ta)
	}
	// Neither → zero time (formats to a valid, if zero, RFC3339).
	if got := volumeCreatedAt(&store.VolumeRecord{}); !got.IsZero() {
		t.Errorf("neither set = %v, want zero", got)
	}
}
