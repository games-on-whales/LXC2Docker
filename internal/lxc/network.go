package lxc

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	DefaultNetworkName = "gow"
	BridgeName         = "br-gow0"
	legacyBridgeName   = "veth0"
	NATTableName       = "gow_nat"
	BridgeCIDR         = "10.100.0.1/24"
	BridgeGW           = "10.100.0.1"
	SubnetMask         = "255.255.255.0"
)

// EnsureBridge creates the managed bridge and assigns it the gateway IP if it
// does not already exist. Idempotent.
func EnsureBridge() error {
	if _, err := net.InterfaceByName(BridgeName); err != nil {
		if err := migrateLegacyBridge(); err != nil {
			return err
		}
	}
	iface, err := net.InterfaceByName(BridgeName)
	if err != nil || iface == nil {
		if out, err := exec.Command("ip", "link", "add", "name", BridgeName, "type", "bridge").CombinedOutput(); err != nil {
			return fmt.Errorf("network: create bridge %s: %s: %w", BridgeName, out, err)
		}
	} else if !isLinuxBridge(BridgeName) {
		return fmt.Errorf("network: interface %s already exists but is not a bridge", BridgeName)
	}
	// Keep the managed bridge independent even if a broad networkd match
	// briefly enslaved it to an appliance bond before this daemon started.
	_ = exec.Command("ip", "link", "set", "dev", BridgeName, "nomaster").Run()
	cmds := [][]string{
		{"ip", "addr", "replace", BridgeCIDR, "dev", BridgeName},
		{"ip", "link", "set", BridgeName, "up"},
	}
	for _, args := range cmds {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("network: %v: %s: %w", args, out, err)
		}
	}

	// Ensure IP forwarding and allow localhost-originated packets to be
	// routed to the bridge (needed for port forwarding from localhost).
	sysctls := []string{
		"net.ipv4.ip_forward=1",
		"net.ipv4.conf.all.route_localnet=1",
		"net.ipv4.conf." + BridgeName + ".route_localnet=1",
	}
	for _, s := range sysctls {
		if out, err := exec.Command("sysctl", "-w", s).CombinedOutput(); err != nil {
			return fmt.Errorf("network: sysctl %s: %s: %w", s, out, err)
		}
	}

	// Ensure the nftables table exists with the masquerade rule.
	// Using "nft -f" with a table block is idempotent — it merges into any
	// existing table rather than replacing it.
	nftRule := fmt.Sprintf(`
table ip %s {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr 10.100.0.0/24 oifname != "%s" masquerade
		ct status dnat masquerade
	}
}
`, NATTableName, BridgeName)
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(nftRule)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("network: nft masquerade: %s: %w", out, err)
	}

	return nil
}

func migrateLegacyBridge() error {
	if legacyBridgeName == BridgeName {
		return nil
	}
	if _, err := net.InterfaceByName(legacyBridgeName); err != nil {
		return nil
	}
	if !isLinuxBridge(legacyBridgeName) {
		return nil
	}
	_ = exec.Command("ip", "link", "set", "dev", legacyBridgeName, "nomaster").Run()
	if out, err := exec.Command("ip", "link", "set", "dev", legacyBridgeName, "down").CombinedOutput(); err != nil {
		return fmt.Errorf("network: bring legacy bridge %s down for rename: %s: %w", legacyBridgeName, out, err)
	}
	if out, err := exec.Command("ip", "link", "set", "dev", legacyBridgeName, "name", BridgeName).CombinedOutput(); err != nil {
		return fmt.Errorf("network: rename legacy bridge %s to %s: %s: %w", legacyBridgeName, BridgeName, out, err)
	}
	return nil
}

func isLinuxBridge(name string) bool {
	if _, err := os.Stat(filepath.Join("/sys/class/net", name, "bridge")); err == nil {
		return true
	}
	return false
}

// TeardownBridge leaves the managed bridge and nftables table in place.
// Called on daemon shutdown. The bridge and NAT rules are left in place
// so that containers that survive the daemon restart keep networking.
// EnsureBridge is idempotent and will reconcile on the next startup.
func TeardownBridge() {
	// Intentionally left as a no-op. Removing the bridge or nft table
	// while containers are running kills their networking. The next
	// EnsureBridge call on startup will reconcile state.
}

// AddPortForward creates an nftables DNAT rule in the managed NAT table to forward
// traffic from hostPort to containerIP:containerPort.
func AddPortForward(containerIP string, hostPort, containerPort int, proto string) error {
	if proto == "" {
		proto = "tcp"
	}
	// prerouting handles traffic from external interfaces; output handles
	// traffic originating on the host itself (e.g. curl localhost:8080).
	nftRule := fmt.Sprintf(`
table ip %s {
	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
		%s dport %d dnat to %s:%d
	}
	chain output {
		type nat hook output priority dstnat; policy accept;
		%s dport %d dnat to %s:%d
	}
}
`, NATTableName, proto, hostPort, containerIP, containerPort,
		proto, hostPort, containerIP, containerPort)

	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(nftRule)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("network: nft port forward: %s: %w", out, err)
	}
	return nil
}

// RemovePortForwards removes all nftables DNAT rules in the managed NAT
// chain that target the given container IP.
func RemovePortForwards(containerIP string) error {
	target := "dnat to " + containerIP + ":"
	// Clean rules from both prerouting and output chains.
	for _, chain := range []string{"prerouting", "output"} {
		out, err := exec.Command("nft", "-a", "list", "chain", "ip", NATTableName, chain).CombinedOutput()
		if err != nil {
			continue // chain may not exist
		}
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.Contains(line, target) {
				continue
			}
			parts := strings.Split(line, "# handle ")
			if len(parts) < 2 {
				continue
			}
			handle := strings.TrimSpace(parts[1])
			exec.Command("nft", "delete", "rule", "ip", NATTableName, chain, "handle", handle).Run()
		}
	}
	return nil
}

// NetworkConfig returns the lxc.conf lines needed to attach a container to
// the managed bridge with the given static IP. Includes a default gateway via the bridge.
func NetworkConfig(ip string) []configItem {
	return []configItem{
		{"lxc.net.0.type", "veth"},
		{"lxc.net.0.link", BridgeName},
		{"lxc.net.0.flags", "up"},
		{"lxc.net.0.ipv4.address", ip + "/24"},
		{"lxc.net.0.ipv4.gateway", BridgeGW},
	}
}

// DualNICConfig returns lxc.conf lines for a dual-NIC container: the LAN
// bridge as net.0 (primary — so mDNS and other services advertise the LAN IP)
// and the internal managed bridge as net.1 (for inter-container traffic).
func DualNICConfig(lanBridge, lanIP, lanGateway, internalIP string) []configItem {
	// net.0 = LAN (primary): routable IP on the physical network.
	items := []configItem{
		{"lxc.net.0.type", "veth"},
		{"lxc.net.0.link", lanBridge},
		{"lxc.net.0.flags", "up"},
		{"lxc.net.0.ipv4.address", lanIP},
	}
	if lanGateway != "" {
		items = append(items, configItem{"lxc.net.0.ipv4.gateway", lanGateway})
	}
	// net.1 = internal managed bridge (no gateway — connected route only).
	items = append(items,
		configItem{"lxc.net.1.type", "veth"},
		configItem{"lxc.net.1.link", BridgeName},
		configItem{"lxc.net.1.flags", "up"},
		configItem{"lxc.net.1.ipv4.address", internalIP + "/24"},
	)
	return items
}
