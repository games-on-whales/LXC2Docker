package api

import (
	"reflect"
	"testing"
)

// TestZfsCandidatePools: the daemon's configured storage/pool must be probed
// first (it's where template datasets actually live), with common fallbacks
// after and no duplication.
func TestZfsCandidatePools(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pveStorage string
		want       []string
	}{
		// The live deployment's pool must be tried, not just the old whitelist.
		{"stores", []string{"stores", "large", "rpool", "tank"}},
		// Empty (legacy/no PVE) → just the fallbacks.
		{"", []string{"large", "rpool", "tank"}},
		// A configured pool that's also a fallback must not be duplicated.
		{"large", []string{"large", "rpool", "tank"}},
		{"rpool", []string{"rpool", "large", "tank"}},
	}
	for _, tc := range cases {
		got := zfsCandidatePools(tc.pveStorage)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("zfsCandidatePools(%q) = %v, want %v", tc.pveStorage, got, tc.want)
		}
	}
}
