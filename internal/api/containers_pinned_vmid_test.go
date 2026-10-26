package api

import (
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// Compose creates a service's replacement before it removes the old container,
// so at create a pinned dld.vmid is still held by the container being replaced.
// Only that holder means the id will be free by the time the new one starts.
func TestReplacedPinnedHolder(t *testing.T) {
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	compose := func(project, service string) map[string]string {
		return map[string]string{"com.docker.compose.project": project, "com.docker.compose.service": service}
	}
	old := &store.ContainerRecord{
		ID: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Name: "0123456789ab_wolf",
		VMID: 104, Labels: compose("wolf-proxmox", "wolf"),
	}
	den := &store.ContainerRecord{
		ID: "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210", Name: "wolf-den",
		VMID: 105, Labels: compose("wolf-proxmox", "wolf-den"),
	}
	for _, rec := range []*store.ContainerRecord{old, den} {
		if err := st.AddContainer(rec); err != nil {
			t.Fatalf("AddContainer: %v", err)
		}
	}

	if got := replacedPinnedHolder(st, 104, compose("wolf-proxmox", "wolf")); got == nil || got.ID != old.ID {
		t.Errorf("same project and service: got %+v, want the old wolf container", got)
	}
	for name, tc := range map[string]struct {
		vmid   int
		labels map[string]string
	}{
		"another service holds the id": {105, compose("wolf-proxmox", "wolf")},
		"another project":              {104, compose("other", "wolf")},
		"no compose labels":            {104, map[string]string{"dld.vmid": "104"}},
		"nil labels":                   {104, nil},
		"nobody holds the id":          {106, compose("wolf-proxmox", "wolf")},
	} {
		if got := replacedPinnedHolder(st, tc.vmid, tc.labels); got != nil {
			t.Errorf("%s: got %s, want nil", name, got.Name)
		}
	}
}
