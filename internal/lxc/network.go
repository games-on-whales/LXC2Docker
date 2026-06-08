package lxc

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// lanDHCPHookPath is the lxc.hook.start-host script that DHCPs the LAN NIC from
// inside the container's network namespace (see writeLANDHCPHook). Lives under
// the daemon state dir so it persists across reboots. A var (not const) so tests
// can redirect it to a temp dir.
var lanDHCPHookPath = "/var/lib/docker-lxc-daemon/lan-dhcp-hook.sh"

const (
	DefaultNetworkName = "veth0"
	BridgeName         = "veth0"
	NATTableName       = "veth_nat"
	BridgeCIDR         = "10.100.0.1/24"
	BridgeGW           = "10.100.0.1"
	SubnetMask         = "255.255.255.0"
)

// EnsureBridge creates the managed bridge and assigns it the gateway IP if it
// does not already exist. Idempotent.
func EnsureBridge() error {
	iface, err := net.InterfaceByName(BridgeName)
	if err != nil || iface == nil {
		// Bridge doesn't exist — create it and assign the gateway IP.
		cmds := [][]string{
			{"ip", "link", "add", "name", BridgeName, "type", "bridge"},
			{"ip", "addr", "add", BridgeCIDR, "dev", BridgeName},
		}
		for _, args := range cmds {
			if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
				return fmt.Errorf("network: %v: %s: %w", args, out, err)
			}
		}
	}

	// Always (re-)assert the bridge is administratively up. A bridge that
	// already existed — e.g. left over from a previous daemon run — may have
	// been brought down; without this, container veths enslave to a DOWN
	// bridge and no traffic flows (container-to-container and host-to-container
	// both fail). Idempotent: "set up" on an already-up link is a no-op.
	if out, err := exec.Command("ip", "link", "set", BridgeName, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("network: bring bridge up: %s: %w", out, err)
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

	// Allow forwarding to and from the managed bridge. A leftover FORWARD policy
	// of DROP — e.g. rules left behind after a previously-installed Docker is
	// purged — blocks container outbound traffic even though NAT masquerade is
	// configured: the container can reach the host but not the internet, so apt
	// and DNS inside containers hang. Insert explicit ACCEPTs at the top of
	// FORWARD so they win over the DROP policy.
	ensureForwardAccept(BridgeName)

	return nil
}

// ensureForwardAccept idempotently inserts iptables FORWARD ACCEPT rules for
// traffic to and from the managed bridge. It is best-effort: failures are
// logged, not fatal, since on hosts without iptables (or without a DROP policy)
// the default FORWARD policy already permits this traffic.
func ensureForwardAccept(bridge string) {
	for _, args := range [][]string{
		{"-i", bridge, "-j", "ACCEPT"},
		{"-o", bridge, "-j", "ACCEPT"},
	} {
		if exec.Command("iptables", append([]string{"-C", "FORWARD"}, args...)...).Run() == nil {
			continue // rule already present — keep this idempotent
		}
		if out, err := exec.Command("iptables", append([]string{"-I", "FORWARD"}, args...)...).CombinedOutput(); err != nil {
			log.Printf("network: iptables FORWARD accept %v failed (continuing): %s: %v",
				args, strings.TrimSpace(string(out)), err)
		}
	}
}

// TeardownBridge removes the managed bridge and nftables table.
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

// resolveLANIP returns the LAN IP in CIDR form for a container. An explicit
// cfg.LANIPRequest (from the "gow.lan.ip" label) overrides the default
// VMID-derived address: a bare IP inherits the bridge subnet, while a value
// that already carries a "/prefixlen" is used as-is. Empty falls back to the
// historical "<prefix>.<vmid>/<subnet>" derivation.
func resolveLANIP(cfg *ContainerConfig, lan LANConfig, vmid int) string {
	if req := strings.TrimSpace(cfg.LANIPRequest); req != "" {
		// "dhcp" (gow.lan.ip=dhcp) requests a DHCP lease on the LAN instead of a
		// daemon-assigned static address; DualNICConfig wires the client hook.
		if strings.EqualFold(req, "dhcp") {
			return "dhcp"
		}
		if strings.Contains(req, "/") {
			return req
		}
		return fmt.Sprintf("%s/%d", req, lan.Subnet)
	}
	return fmt.Sprintf("%s.%d/%d", lan.Prefix, vmid, lan.Subnet)
}

// applyLANNetworking gives a container a routable NIC on the physical LAN
// bridge when one is configured. It fires in two cases:
//
//   - an explicit gow.lan=true label (cfg.LAN), and
//   - any container that requested Docker --network=host.
//
// Host networking can't be honored on a Proxmox CT: a CT can't share the
// host's network namespace, so a host-mode container would otherwise come up
// with an empty, unreachable netns — no addresses, no default route, and mDNS
// failing with "Failed to open any client sockets" (see issue #53, Wolf). The
// LAN bridge is the Proxmox-appropriate way to put a container on the host's
// network, where inbound traffic and mDNS/Moonlight discovery work, so we
// convert host mode into the dual-NIC LAN setup. Returns true if it applied.
func applyLANNetworking(cfg *ContainerConfig, lan LANConfig, vmid int) bool {
	if lan.Bridge == "" || (!cfg.LAN && cfg.NetworkMode != "host") {
		return false
	}
	// A real LAN NIC replaces host-netns sharing; clearing the mode also stops
	// buildItems/buildPVEItems from emitting an lxc.namespace.clone that would
	// (futilely) try to inherit the host network namespace.
	cfg.NetworkMode = ""
	cfg.LANBridge = lan.Bridge
	cfg.LANIP = resolveLANIP(cfg, lan, vmid)
	cfg.LANGateway = lan.Gateway
	log.Printf("CreateContainer[PVE]: LAN NIC on %s with IP %s", cfg.LANBridge, cfg.LANIP)
	return true
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
	}
	if lanIP == "dhcp" {
		// DHCP: bring the NIC up with a known name but no static address, and
		// attach a start-host hook that runs a DHCP client inside the container's
		// netns (the unmanaged gow images ship no DHCP client of their own).
		items = append(items, configItem{"lxc.net.0.name", "eth0"})
		if err := writeLANDHCPHook(); err != nil {
			log.Printf("DualNICConfig: LAN DHCP hook write failed: %v (LAN NIC will come up without an address)", err)
		} else {
			items = append(items, configItem{"lxc.hook.start-host", lanDHCPHookPath})
		}
	} else {
		items = append(items, configItem{"lxc.net.0.ipv4.address", lanIP})
		if lanGateway != "" {
			items = append(items, configItem{"lxc.net.0.ipv4.gateway", lanGateway})
		}
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

// lanDHCPHookScript runs as an lxc.hook.start-host hook: it DHCPs the LAN NIC
// (eth0) from inside the container's network namespace using the host's dhcpcd
// (the unmanaged gow images have no DHCP client). --nohook resolv.conf keeps it
// from rewriting the host's DNS config; dhcpcd daemonizes to renew the lease.
const lanDHCPHookScript = `#!/bin/sh
[ -n "$LXC_PID" ] || exit 0
command -v dhcpcd >/dev/null 2>&1 || exit 0
# One-shot lease (-1): dhcpcd exits right after configuring the address, so it
# leaves no lingering host-wide master to collide with the next container start
# (dhcpcd keys its control state on the interface name, which repeats as "eth0"
# across netns). The lease lands within ~1-2s — before Wolf's much later mDNS
# bind — so the LAN address is advertised. Background so we don't delay startup.
nsenter -t "$LXC_PID" -n dhcpcd -1 -q --nohook resolv.conf eth0 >/dev/null 2>&1 &
exit 0
`

// writeLANDHCPHook writes the DHCP start-host hook script idempotently.
func writeLANDHCPHook() error {
	if err := os.MkdirAll(filepath.Dir(lanDHCPHookPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(lanDHCPHookPath, []byte(lanDHCPHookScript), 0o755)
}
