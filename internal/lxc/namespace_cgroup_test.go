package lxc

import (
	"strings"
	"testing"
)

// TestBuildItemsClonesCgroupNamespace ensures a container that shares a
// host namespace still gets its own cgroup namespace, so cgroup:mixed can
// mount the cgroup v2 hierarchy without an unsupported force-mount.
func TestBuildItemsClonesCgroupNamespace(t *testing.T) {
	items := buildItems(&ContainerConfig{IpcMode: "host"}, "10.0.0.2")
	var clone string
	for _, it := range items {
		if it.key == "lxc.namespace.clone" {
			clone = it.value
		}
	}
	if clone == "" {
		t.Fatal("expected lxc.namespace.clone to be set for a host-ipc container")
	}
	if !strings.Contains(clone, "cgroup") {
		t.Errorf("namespace.clone %q must include cgroup", clone)
	}
}

// TestBuildPVEItemsUsesCgroupMixed guards the regression where cgroup:rw made a
// freshly-created uinput device stat() as 0:0 on the PVE path (controllers went
// undetected). cgroup:mixed keeps nested-runtime cgroup access without that.
func TestBuildPVEItemsUsesCgroupMixed(t *testing.T) {
	items := buildPVEItems(&ContainerConfig{}, "10.0.0.2")
	var sawMixed, sawRW bool
	for _, it := range items {
		if it.key != "lxc.mount.auto" {
			continue
		}
		if strings.Contains(it.value, "cgroup:mixed") {
			sawMixed = true
		}
		if strings.Contains(it.value, "cgroup:rw") {
			sawRW = true
		}
	}
	if !sawMixed {
		t.Error("buildPVEItems must auto-mount cgroup:mixed")
	}
	if sawRW {
		t.Error("buildPVEItems must not use cgroup:rw (breaks uinput device-node major:minor)")
	}
}
