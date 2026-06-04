package lxc

import (
	"strings"
	"testing"
)

// TestBuildItemsClonesCgroupNamespace ensures a container that shares a
// host namespace still gets its own cgroup namespace, so cgroup:rw can
// mount the cgroup v2 unified hierarchy without an unsupported force-mount.
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
