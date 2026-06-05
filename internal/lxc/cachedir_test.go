package lxc

import "testing"

func TestResolveCacheDir(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                              string
		explicit, statePath, stg, stgType string
		want                              string
	}{
		{"explicit wins", "/big/cache", "/var/lib/dld", "storage", "zfspool", "/big/cache"},
		{"explicit wins over block", "/big/cache", "/var/lib/dld", "tank", "lvmthin", "/big/cache"},
		{"zfs auto-default onto pool", "", "/var/lib/dld", "stores", "zfspool", "/stores/docker-lxc-daemon-cache"},
		{"lvmthin falls back to state", "", "/var/lib/dld", "storage", "lvmthin", "/var/lib/dld"},
		{"legacy (no pve) uses state", "", "/var/lib/dld", "", "", "/var/lib/dld"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveCacheDir(tc.explicit, tc.statePath, tc.stg, tc.stgType); got != tc.want {
				t.Fatalf("resolveCacheDir(%q,%q,%q,%q) = %q, want %q",
					tc.explicit, tc.statePath, tc.stg, tc.stgType, got, tc.want)
			}
		})
	}
}
