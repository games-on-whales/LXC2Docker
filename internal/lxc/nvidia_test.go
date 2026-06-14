package lxc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCDISymlinks(t *testing.T) {
	t.Parallel()
	args := []string{
		"nvidia-cdi-hook", "create-symlinks",
		"--link", "libcuda.so.1::/usr/lib/x86_64-linux-gnu/libcuda.so",
		"--link", "../libnvidia-allocator.so.1::/usr/lib/x86_64-linux-gnu/gbm/nvidia-drm_gbm.so",
	}
	got := parseCDISymlinks(args)
	want := []string{
		"libcuda.so.1::/usr/lib/x86_64-linux-gnu/libcuda.so",
		"../libnvidia-allocator.so.1::/usr/lib/x86_64-linux-gnu/gbm/nvidia-drm_gbm.so",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d links, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("link[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNvidiaItemsFromSpec(t *testing.T) {
	t.Parallel()
	spec := &cdiSpec{
		ContainerEdits: cdiEdits{
			Mounts: []cdiMount{
				{HostPath: "/usr/lib/x86_64-linux-gnu/libcuda.so.610.43.02", ContainerPath: "/usr/lib/x86_64-linux-gnu/libcuda.so.610.43.02"},
				{HostPath: "/usr/bin/nvidia-smi", ContainerPath: "/usr/bin/nvidia-smi"},
			},
			DeviceNodes: []cdiDeviceNode{
				{Path: "/dev/nvidiactl", Major: 195, Minor: 255},
			},
			Hooks: []cdiHook{
				{HookName: "createContainer", Args: []string{"nvidia-cdi-hook", "create-symlinks", "--link", "libcuda.so.610.43.02::/usr/lib/x86_64-linux-gnu/libcuda.so.1"}},
			},
		},
		Devices: []cdiDevice{
			{Name: "0", ContainerEdits: cdiEdits{DeviceNodes: []cdiDeviceNode{{Path: "/dev/nvidia0", Major: 195, Minor: 0}}}},
			{Name: "all", ContainerEdits: cdiEdits{DeviceNodes: []cdiDeviceNode{
				{Path: "/dev/nvidia0", Major: 195, Minor: 0},
				{Path: "/dev/dri/renderD128", Major: 226, Minor: 128},
			}}},
		},
	}

	items, links := nvidiaItemsFromSpec(spec)

	has := func(key, substr string) bool {
		for _, it := range items {
			if it.key == key && strings.Contains(it.value, substr) {
				return true
			}
		}
		return false
	}

	// Driver lib + binary mounts (bind, ro).
	if !has("lxc.mount.entry", "libcuda.so.610.43.02 usr/lib/x86_64-linux-gnu/libcuda.so.610.43.02 none bind,create=file,ro") {
		t.Fatalf("missing libcuda mount, items=%#v", items)
	}
	if !has("lxc.mount.entry", "/usr/bin/nvidia-smi usr/bin/nvidia-smi none bind,create=file,ro") {
		t.Fatalf("missing nvidia-smi mount")
	}
	// Device cgroup allow + node bind (from global edits and the "all" device).
	if !has("lxc.cgroup2.devices.allow", "c 195:255 rwm") {
		t.Fatalf("missing nvidiactl cgroup allow")
	}
	if !has("lxc.cgroup2.devices.allow", "c 226:128 rwm") {
		t.Fatalf("missing renderD128 cgroup allow (from 'all' device)")
	}
	if !has("lxc.mount.entry", "/dev/nvidia0 dev/nvidia0 none bind,create=file 0 0") {
		t.Fatalf("missing nvidia0 device node bind")
	}
	// The "0" device's edits must NOT be applied (only global + "all").
	devCount := 0
	for _, it := range items {
		if it.key == "lxc.mount.entry" && strings.Contains(it.value, "dev/nvidia0") {
			devCount++
		}
	}
	if devCount != 1 {
		t.Fatalf("nvidia0 should be bound once (global+all dedup), got %d", devCount)
	}
	// Symlink pair collected for the hook.
	if len(links) != 1 || links[0] != "libcuda.so.610.43.02::/usr/lib/x86_64-linux-gnu/libcuda.so.1" {
		t.Fatalf("unexpected links: %#v", links)
	}
}

func TestWriteNvidiaMountHookContent(t *testing.T) {
	// Redirect the hook path to a temp dir via a small indirection: write to a
	// temp file and assert content. We can't easily override the const, so test
	// the generation by writing to the real path only if writable; otherwise
	// validate the script body via a local copy of the logic is overkill —
	// instead assert the generated file content when MkdirAll succeeds.
	dir := t.TempDir()
	path := filepath.Join(dir, "hook.sh")
	links := []string{
		"libcuda.so.1::/usr/lib/x86_64-linux-gnu/libcuda.so",
		"../libnvidia-allocator.so.1::/usr/lib/x86_64-linux-gnu/gbm/nvidia-drm_gbm.so",
		"bad-entry-no-separator",
	}
	if err := writeNvidiaMountHookTo(path, links); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		`R="$LXC_ROOTFS_MOUNT"`,
		`ln -sf "libcuda.so.1" "$R/usr/lib/x86_64-linux-gnu/libcuda.so"`,
		`ln -sf "../libnvidia-allocator.so.1" "$R/usr/lib/x86_64-linux-gnu/gbm/nvidia-drm_gbm.so"`,
		`ldconfig`,
		"exit 0",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("hook script missing %q\n---\n%s", want, s)
		}
	}
	if strings.Contains(s, "bad-entry-no-separator") {
		t.Fatalf("malformed link should be skipped")
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("hook script not executable: %v", fi.Mode())
	}
}
