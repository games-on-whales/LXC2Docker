package lxc

import "testing"

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
