package lxc

import (
	"strings"
	"testing"
)

func TestCapabilityItemsPrivileged(t *testing.T) {
	t.Parallel()

	items := capabilityItems(&ContainerConfig{Privileged: true})
	if len(items) != 2 {
		t.Fatalf("expected 2 capability items, got %d", len(items))
	}
	if items[0] != (configItem{key: "lxc.cap.drop", value: ""}) {
		t.Fatalf("unexpected first item: %#v", items[0])
	}
	if items[1] != (configItem{key: "lxc.cgroup2.devices.allow", value: "a"}) {
		t.Fatalf("unexpected second item: %#v", items[1])
	}
}

func TestCapabilityItemsUsesKeepListForCapAddAndDrop(t *testing.T) {
	t.Parallel()

	items := capabilityItems(&ContainerConfig{
		CapAdd:  []string{"NET_ADMIN"},
		CapDrop: []string{"NET_RAW"},
	})
	if len(items) == 0 {
		t.Fatal("expected capability items")
	}
	if items[0] != (configItem{key: "lxc.cap.drop", value: ""}) {
		t.Fatalf("expected inherited drops to be cleared first, got %#v", items[0])
	}

	var sawNetAdmin bool
	var sawNetRaw bool
	for _, item := range items[1:] {
		if item.key != "lxc.cap.keep" {
			t.Fatalf("expected keep-list entries, got %#v", item)
		}
		if item.value == "net_admin" {
			sawNetAdmin = true
		}
		if item.value == "net_raw" {
			sawNetRaw = true
		}
	}
	if !sawNetAdmin {
		t.Fatal("expected NET_ADMIN to be present in final keep-list")
	}
	if sawNetRaw {
		t.Fatal("expected NET_RAW to be removed from final keep-list")
	}
}

func TestNormalizeCap(t *testing.T) {
	t.Parallel()

	if got := normalizeCap(" CAP_NET_ADMIN "); got != "net_admin" {
		t.Fatalf("normalizeCap = %q, want %q", got, "net_admin")
	}
}

func TestAppendSocketMountMountsRuntimeSocketDirAtRealDestination(t *testing.T) {
	t.Parallel()

	runtimeDir := "/run/user/wolf"
	socket := "/run/user/wolf/wayland-1"

	cfg := &ContainerConfig{}
	items := appendSocketMount(nil, cfg, socket, MountSpec{
		Source:      socket,
		Destination: "/run/user/wolf/wayland-1",
	})

	want := strings.ReplaceAll(runtimeDir, " ", `\040`) + " run/user/wolf none bind,create=dir 0 0"
	if !hasMountEntry(items, want) {
		t.Fatalf("expected direct runtime dir mount %q, got %#v", want, items)
	}
	if len(cfg.SocketLinks) != 0 {
		t.Fatalf("expected no socket symlinks for direct runtime dir mount, got %#v", cfg.SocketLinks)
	}
}

func TestAppendSocketMountKeepsHiddenSocketMountForTranslatedDestinations(t *testing.T) {
	t.Parallel()

	runtimeDir := "/run/wolf"
	socket := "/run/wolf/wolf.sock"

	cfg := &ContainerConfig{}
	items := appendSocketMount(nil, cfg, socket, MountSpec{
		Source:      socket,
		Destination: "/var/run/wolf/wolf.sock",
	})

	want := strings.ReplaceAll(runtimeDir, " ", `\040`) + " .socket-dirs/wolf none bind,create=dir 0 0"
	if !hasMountEntry(items, want) {
		t.Fatalf("expected hidden socket dir mount %q, got %#v", want, items)
	}
	if got := cfg.SocketLinks["/var/run/wolf/wolf.sock"]; got != "/.socket-dirs/wolf/wolf.sock" {
		t.Fatalf("unexpected socket link target %q", got)
	}
}

func hasMountEntry(items []configItem, want string) bool {
	for _, item := range items {
		if item.key == "lxc.mount.entry" && item.value == want {
			return true
		}
	}
	return false
}
