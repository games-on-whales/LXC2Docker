package lxc

import (
	"strings"
	"testing"
)

func lanCfg() LANConfig {
	return LANConfig{Bridge: "vmbr0", Prefix: "192.168.96", Gateway: "192.168.96.1", Subnet: 22}
}

// TestApplyLANNetworkingConvertsHostMode covers issue #53: a --network=host
// container can't share the host netns on a Proxmox CT, so when a LAN bridge
// is configured it must be given a routable LAN NIC instead of being left with
// an empty, unreachable network namespace.
func TestApplyLANNetworkingConvertsHostMode(t *testing.T) {
	cfg := &ContainerConfig{NetworkMode: "host"}
	if !applyLANNetworking(cfg, lanCfg(), 1000, "02:11:22:33:44:55") {
		t.Fatal("expected applyLANNetworking to apply for a host-mode container")
	}
	if cfg.NetworkMode == "host" {
		t.Error("NetworkMode should be cleared so no host-netns clone is emitted")
	}
	if cfg.LANBridge != "vmbr0" {
		t.Errorf("LANBridge = %q, want vmbr0", cfg.LANBridge)
	}
	if cfg.LANIP != "192.168.96.1000/22" {
		t.Errorf("LANIP = %q, want 192.168.96.1000/22", cfg.LANIP)
	}
	if cfg.LANMacAddress != "02:11:22:33:44:55" {
		t.Errorf("LANMacAddress = %q, want 02:11:22:33:44:55", cfg.LANMacAddress)
	}

	// The generated config must carry the dual-NIC LAN interface and no
	// lxc.namespace.clone trying to inherit the host network namespace.
	items := buildPVEItems(cfg, "10.100.0.5")
	var hasLANLink, hasLANMAC, hasNSClone bool
	for _, it := range items {
		if it.key == "lxc.net.0.link" && it.value == "vmbr0" {
			hasLANLink = true
		}
		if it.key == "lxc.net.0.hwaddr" && it.value == "02:11:22:33:44:55" {
			hasLANMAC = true
		}
		if it.key == "lxc.namespace.clone" {
			hasNSClone = true
		}
	}
	if !hasLANLink {
		t.Error("expected a LAN NIC (lxc.net.0.link = vmbr0) in the generated config")
	}
	if !hasLANMAC {
		t.Error("expected a stable LAN MAC (lxc.net.0.hwaddr) in the generated config")
	}
	if hasNSClone {
		t.Error("a converted host-mode container must not emit lxc.namespace.clone")
	}
}

// TestApplyLANNetworkingExplicitLabel covers the existing gow.lan=true path.
func TestApplyLANNetworkingExplicitLabel(t *testing.T) {
	cfg := &ContainerConfig{LAN: true}
	if !applyLANNetworking(cfg, lanCfg(), 1001, "02:aa:bb:cc:dd:ee") {
		t.Fatal("expected applyLANNetworking to apply for a gow.lan container")
	}
	if cfg.LANBridge != "vmbr0" || !strings.HasPrefix(cfg.LANIP, "192.168.96.") {
		t.Errorf("unexpected LAN config: bridge=%q ip=%q", cfg.LANBridge, cfg.LANIP)
	}
}

// TestApplyLANNetworkingNoBridge ensures host mode is left untouched when no
// LAN bridge is configured (nothing to convert it to).
func TestApplyLANNetworkingNoBridge(t *testing.T) {
	cfg := &ContainerConfig{NetworkMode: "host"}
	if applyLANNetworking(cfg, LANConfig{}, 1000, "02:11:22:33:44:55") {
		t.Fatal("expected applyLANNetworking to be a no-op without a LAN bridge")
	}
	if cfg.NetworkMode != "host" {
		t.Errorf("NetworkMode = %q, want host (unchanged)", cfg.NetworkMode)
	}
	if cfg.LANBridge != "" {
		t.Errorf("LANBridge = %q, want empty", cfg.LANBridge)
	}
}

// TestApplyLANNetworkingSkipsBridgeContainer ensures an ordinary bridge-mode
// container (no host mode, no label) is untouched even when a bridge exists.
func TestApplyLANNetworkingSkipsBridgeContainer(t *testing.T) {
	cfg := &ContainerConfig{}
	if applyLANNetworking(cfg, lanCfg(), 1000, "02:11:22:33:44:55") {
		t.Fatal("expected applyLANNetworking to skip a plain bridge container")
	}
	if cfg.LANBridge != "" {
		t.Errorf("LANBridge = %q, want empty", cfg.LANBridge)
	}
}

func TestStableLANMac(t *testing.T) {
	id := "271453ba95aad71eb253fb3f130e342b206ea8501ee263685012216c10333132"
	got := stableLANMac(id)
	if got != stableLANMac(id) {
		t.Fatal("stableLANMac changed for the same ID")
	}
	if !strings.HasPrefix(got, "02:") {
		t.Fatalf("stableLANMac = %q, want locally-administered unicast prefix 02", got)
	}
	if strings.Count(got, ":") != 5 {
		t.Fatalf("stableLANMac = %q, want MAC format", got)
	}
}
