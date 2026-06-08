package lxc

import (
	"os"
	"path/filepath"
	"strings"
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
	// Redirect the hook path to a temp file so the write succeeds in the test env.
	old := lanDHCPHookPath
	lanDHCPHookPath = filepath.Join(t.TempDir(), "lan-dhcp-hook.sh")
	defer func() { lanDHCPHookPath = old }()

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

	if v := get("lxc.net.0.ipv4.address"); len(v) != 0 {
		t.Fatalf("DHCP LAN NIC must not have a static address, got %v", v)
	}
	if v := get("lxc.net.0.name"); len(v) != 1 || v[0] != "eth0" {
		t.Fatalf("expected lxc.net.0.name=eth0, got %v", v)
	}
	if v := get("lxc.hook.start-host"); len(v) != 1 || v[0] != lanDHCPHookPath {
		t.Fatalf("expected start-host hook %q, got %v", lanDHCPHookPath, v)
	}
	// Internal NIC still static.
	if v := get("lxc.net.1.ipv4.address"); len(v) != 1 || v[0] != "10.100.0.5/24" {
		t.Fatalf("expected internal NIC 10.100.0.5/24, got %v", v)
	}
	// Hook script written and runs a DHCP client in the netns.
	data, err := os.ReadFile(lanDHCPHookPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "dhcpcd") || !strings.Contains(string(data), "$LXC_PID") {
		t.Fatalf("hook script missing dhcpcd/$LXC_PID:\n%s", data)
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
