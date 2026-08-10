package lxc

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeCluster points the guest-id scan at a temporary /etc/pve lookalike and
// writes one <id>.conf per entry. Keys are paths relative to the fake /etc/pve
// root, so a test spells out exactly which node owns which guest.
func fakeCluster(t *testing.T, confs ...string) {
	t.Helper()
	root := t.TempDir()
	for _, rel := range confs {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	saved := pveGuestGlobs
	pveGuestGlobs = []string{
		filepath.Join(root, "nodes/*/lxc/*.conf"),
		filepath.Join(root, "nodes/*/qemu-server/*.conf"),
		filepath.Join(root, "lxc/*.conf"),
		filepath.Join(root, "qemu-server/*.conf"),
	}
	savedReserved := vmidReserved
	vmidReserved = map[int]bool{}
	t.Cleanup(func() { pveGuestGlobs = saved; vmidReserved = savedReserved })
}

// The bug this guards: scanning only the local node's config dir hands out ids a
// peer already owns, and `pct create` then refuses them ("CT 103 already exists
// on node 'pve1'"), which the caller sees as a container that will not start.
func TestUsedVMIDsSeesEveryNodeInTheCluster(t *testing.T) {
	fakeCluster(t,
		"nodes/mjolnir/lxc/100.conf",
		"nodes/pve1/lxc/101.conf",
		"nodes/pve1/qemu-server/102.conf",
	)

	used, err := usedVMIDs()
	if err != nil {
		t.Fatalf("usedVMIDs: %v", err)
	}
	for _, id := range []int{100, 101, 102} {
		if !used[id] {
			t.Errorf("id %d owned by a node in the cluster, but reported free", id)
		}
	}
	if used[103] {
		t.Errorf("id 103 is owned by nobody, but reported used")
	}
}

// A node that is not in a cluster has no nodes/ tree at all — only the local
// symlinks — and must still be read.
func TestUsedVMIDsCoversASingleNode(t *testing.T) {
	fakeCluster(t, "lxc/100.conf", "qemu-server/101.conf")

	used, err := usedVMIDs()
	if err != nil {
		t.Fatalf("usedVMIDs: %v", err)
	}
	if !used[100] || !used[101] {
		t.Errorf("local guests missed: %v", used)
	}
}

func TestAllocateVMIDSkipsIdsOwnedByAPeer(t *testing.T) {
	fakeCluster(t,
		"nodes/mjolnir/lxc/100.conf",
		"nodes/pve1/lxc/101.conf",
		"nodes/pve1/qemu-server/102.conf",
	)

	got, err := allocateVMID(0)
	if err != nil {
		t.Fatalf("allocateVMID: %v", err)
	}
	if got != 103 {
		t.Errorf("allocateVMID = %d, want 103 (the lowest id no node owns)", got)
	}
}

func TestAllocateVMIDHonoursARequestedID(t *testing.T) {
	fakeCluster(t, "nodes/pve1/lxc/101.conf")

	got, err := allocateVMID(250)
	if err != nil {
		t.Fatalf("allocateVMID(250): %v", err)
	}
	if got != 250 {
		t.Errorf("allocateVMID(250) = %d, want the id that was asked for", got)
	}
}

// Sliding to the next free id would hand back a CT at an id the caller did not
// ask for, which defeats the point of pinning one.
func TestAllocateVMIDRefusesARequestedIDThatIsTaken(t *testing.T) {
	fakeCluster(t, "nodes/pve1/qemu-server/250.conf")

	got, err := allocateVMID(250)
	if err == nil {
		t.Fatalf("allocateVMID(250) = %d, want an error: a peer's VM owns 250", got)
	}
}

// Two concurrent creates pinned to the same id: the second must fail rather than
// race the first onto a CT that is still being written to disk.
func TestAllocateVMIDRefusesARequestedIDAlreadyInFlight(t *testing.T) {
	fakeCluster(t)

	if _, err := allocateVMID(250); err != nil {
		t.Fatalf("first allocateVMID(250): %v", err)
	}
	if got, err := allocateVMID(250); err == nil {
		t.Fatalf("second allocateVMID(250) = %d, want an error: still in flight", got)
	}
}

// A create that fails before writing the CT config must leave its id free. Left
// reserved, a pinned id would fail every later attempt with "already being
// created" until the daemon restarted, so one transient failure would
// permanently wedge a container that restarts — exactly the situation pinning an
// id is used in.
func TestReleaseVMIDLetsAFailedCreateRetryTheSameID(t *testing.T) {
	fakeCluster(t)

	vmid, err := allocateVMID(250)
	if err != nil {
		t.Fatalf("allocateVMID(250): %v", err)
	}
	releaseVMID(vmid) // the create failed; nothing was written to disk

	if got, err := allocateVMID(250); err != nil {
		t.Fatalf("retry of allocateVMID(250) = %d, %v; want the id back", got, err)
	}
}
