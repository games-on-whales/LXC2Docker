package lxc

import (
	"strings"
	"testing"
)

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
		},
		Devices: []cdiDevice{
			{Name: "0", ContainerEdits: cdiEdits{DeviceNodes: []cdiDeviceNode{{Path: "/dev/nvidia0", Major: 195, Minor: 0}}}},
			{Name: "all", ContainerEdits: cdiEdits{DeviceNodes: []cdiDeviceNode{
				{Path: "/dev/nvidia0", Major: 195, Minor: 0},
				{Path: "/dev/dri/renderD128", Major: 226, Minor: 128},
			}}},
		},
	}

	items := nvidiaItemsFromSpec(spec)

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
	// No lxc.hook.* — PVE rejects them; symlinks/ldcache are handled by the
	// image's nvidia init.
	for _, it := range items {
		if strings.HasPrefix(it.key, "lxc.hook.") {
			t.Fatalf("unexpected lxc hook emitted: %s = %s", it.key, it.value)
		}
	}
}
