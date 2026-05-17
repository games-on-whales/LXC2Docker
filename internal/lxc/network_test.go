package lxc

import (
	"testing"
)

func TestManagedNetworkNamesUseVethBridge(t *testing.T) {
	if DefaultNetworkName != "veth" {
		t.Fatalf("DefaultNetworkName = %q, want veth", DefaultNetworkName)
	}
	if BridgeName != "veth0" {
		t.Fatalf("BridgeName = %q, want veth0", BridgeName)
	}
}

func TestNetworkConfigUsesManagedBridge(t *testing.T) {
	items := NetworkConfig("10.100.0.42")
	if got := configValue(items, "lxc.net.0.link"); got != BridgeName {
		t.Fatalf("lxc.net.0.link = %q, want %q", got, BridgeName)
	}
	if got := configValue(items, "lxc.net.0.ipv4.gateway"); got != BridgeGW {
		t.Fatalf("gateway = %q, want %q", got, BridgeGW)
	}
}

func TestDualNICConfigUsesManagedBridgeForInternalNIC(t *testing.T) {
	items := DualNICConfig("vmbr0", "192.168.1.120/24", "192.168.1.1", "10.100.0.42")
	if got := configValue(items, "lxc.net.0.link"); got != "vmbr0" {
		t.Fatalf("LAN link = %q, want vmbr0", got)
	}
	if got := configValue(items, "lxc.net.1.link"); got != BridgeName {
		t.Fatalf("internal link = %q, want %q", got, BridgeName)
	}
}

func configValue(items []configItem, key string) string {
	for _, item := range items {
		if item.key == key {
			return item.value
		}
	}
	return ""
}
