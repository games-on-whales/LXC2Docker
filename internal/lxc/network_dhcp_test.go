package lxc

import (
	"testing"
)

func TestResolveLANIPDHCP(t *testing.T) {
	t.Parallel()
	cfg := &ContainerConfig{LANIPRequest: "dhcp"}
	if got := resolveLANIP(cfg, LANConfig{Prefix: "192.168.1", Subnet: 23}, 104); got != "dhcp" {
		t.Fatalf("resolveLANIP(dhcp) = %q, want dhcp", got)
	}
	cfgUpper := &ContainerConfig{LANIPRequest: "DHCP"}
	if got := resolveLANIP(cfgUpper, LANConfig{Prefix: "192.168.1", Subnet: 23}, 104); got != "dhcp" {
		t.Fatalf("resolveLANIP(DHCP) = %q, want dhcp (case-insensitive)", got)
	}
	// Static still works.
	cfgStatic := &ContainerConfig{LANIPRequest: "192.168.1.50"}
	if got := resolveLANIP(cfgStatic, LANConfig{Prefix: "192.168.1", Subnet: 23}, 104); got != "192.168.1.50/23" {
		t.Fatalf("resolveLANIP(static) = %q, want 192.168.1.50/23", got)
	}
}

func TestDualNICConfigDHCP(t *testing.T) {
	t.Parallel()
	items := DualNICConfig("vmbr1", "dhcp", "192.168.1.1", "10.100.0.5")

	get := func(key string) []string {
		var vals []string
		for _, it := range items {
			if it.key == key {
				vals = append(vals, it.value)
			}
		}
		return vals
	}

	// DHCP LAN NIC: named eth0, no static address (daemon leases it post-start).
	if v := get("lxc.net.0.ipv4.address"); len(v) != 0 {
		t.Fatalf("DHCP LAN NIC must not have a static address, got %v", v)
	}
	if v := get("lxc.net.0.name"); len(v) != 1 || v[0] != "eth0" {
		t.Fatalf("expected lxc.net.0.name=eth0, got %v", v)
	}
	// No lxc.hook.* (PVE rejects them; DHCP is daemon-driven).
	if v := get("lxc.hook.start-host"); len(v) != 0 {
		t.Fatalf("expected no start-host hook, got %v", v)
	}
	// Internal NIC still static.
	if v := get("lxc.net.1.ipv4.address"); len(v) != 1 || v[0] != "10.100.0.5/24" {
		t.Fatalf("expected internal NIC 10.100.0.5/24, got %v", v)
	}

	// Static path unchanged: address present, no hook.
	st := DualNICConfig("vmbr1", "192.168.1.50/23", "192.168.1.1", "10.100.0.5")
	hasAddr, hasHook := false, false
	for _, it := range st {
		if it.key == "lxc.net.0.ipv4.address" && it.value == "192.168.1.50/23" {
			hasAddr = true
		}
		if it.key == "lxc.hook.start-host" {
			hasHook = true
		}
	}
	if !hasAddr || hasHook {
		t.Fatalf("static DualNIC: hasAddr=%v hasHook=%v (want true,false)", hasAddr, hasHook)
	}
}
