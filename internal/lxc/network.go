package lxc

import (
	"crypto/sha256"
	"fmt"
	"log"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// nftMu serializes every mutation of the managed NAT table's DNAT rules
// (AddPortForward / RemovePortForwards / RemovePortForwardForHostPort). nft
// itself serializes at the kernel, but each of these performs a multi-step
// evict-then-add (or list-then-delete) sequence; without this lock the periodic
// reconciler and the API lifecycle handlers could interleave their phases — e.g.
// a stop's remove slipping between AddPortForward's evict and add — and leave the
// table in a state neither intended. Holding one process-wide lock across each
// full sequence makes them atomic with respect to each other.
var nftMu sync.Mutex

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
//
// It is self-healing: any pre-existing DNAT rule for the same hostPort/proto is
// removed first, regardless of which container IP it targets. nft "-f" with chain
// blocks *appends* rules, so without this a re-publish (e.g. a container restart
// that lands on a new IP, or a daemon restart that recreates the container) would
// stack a fresh rule *behind* the stale one — and nft matches the first rule, so
// traffic would keep being DNAT'd to the old, now-dead container IP (the port
// becomes unreachable). Purging by hostPort guarantees exactly one live rule
// pointing at the current IP.
func AddPortForward(containerIP string, hostPort, containerPort int, proto string) error {
	nftMu.Lock()
	defer nftMu.Unlock()
	if proto == "" {
		proto = "tcp"
	}
	// Clear any stale rule for this hostPort before inserting the fresh one so a
	// re-publish to a new container IP can't be shadowed by a leftover rule.
	if err := removePortForwardsForHostPort(hostPort, proto); err != nil {
		return fmt.Errorf("network: clear stale port forward for %s/%d: %w", proto, hostPort, err)
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
// chain that target the given container IP. Used on container stop to release
// that container's published-port rules.
func RemovePortForwards(containerIP string) error {
	nftMu.Lock()
	defer nftMu.Unlock()
	return deleteMatchingNATRules(func(line string) bool {
		return strings.Contains(line, "dnat to "+containerIP+":")
	})
}

// RemovePortForwardForHostPort removes every DNAT rule publishing the given
// hostPort/proto, whatever container IP it targets. The periodic reconciler uses
// it to prune ORPHAN rules — a published port that no running container claims
// any more (e.g. left behind when a stop's remove raced a reconcile re-add, or a
// container that vanished from the store). Distinct from the unexported
// removePortForwardsForHostPort only in that it takes the nftMu (the unexported
// one runs inside AddPortForward, which already holds it).
func RemovePortForwardForHostPort(hostPort int, proto string) error {
	nftMu.Lock()
	defer nftMu.Unlock()
	return removePortForwardsForHostPort(hostPort, proto)
}

// removePortForwardsForHostPort removes any DNAT rule in the managed NAT chains
// that publishes the given hostPort/proto, whatever container IP it targets.
// Used by AddPortForward to make re-publishing a port idempotent and to evict
// rules left behind pointing at a stale (restarted) container's old IP.
func removePortForwardsForHostPort(hostPort int, proto string) error {
	return deleteMatchingNATRules(func(line string) bool {
		return natLineMatchesHostPort(line, hostPort, proto)
	})
}

// natLineMatchesHostPort reports whether an nft rule listing line is a DNAT rule
// publishing hostPort/proto. nft prints e.g.
//
//	tcp dport 47989 dnat to 10.100.0.22:47989 # handle 7
//
// The trailing space after the port number is significant: it stops 47989 from
// matching 479890 (or 147989).
func natLineMatchesHostPort(line string, hostPort int, proto string) bool {
	needle := fmt.Sprintf("%s dport %d ", proto, hostPort)
	return strings.Contains(line, needle) && strings.Contains(line, "dnat to ")
}

// natHandlesToDelete parses an "nft -a list chain" listing and returns the
// handles of every rule whose line satisfies match. Pure (no exec) so the
// match/parse logic is unit-testable without nft or root.
func natHandlesToDelete(listing string, match func(line string) bool) []string {
	var handles []string
	for _, line := range strings.Split(listing, "\n") {
		if !match(line) {
			continue
		}
		parts := strings.Split(line, "# handle ")
		if len(parts) < 2 {
			continue
		}
		if h := strings.TrimSpace(parts[1]); h != "" {
			handles = append(handles, h)
		}
	}
	return handles
}

// parseDNATChain extracts the published-port DNAT mappings from one nft chain
// listing. It returns rules as map["<proto>/<hostPort>"] = "<targetIP>:<targetPort>"
// plus the set of keys that appear MORE THAN ONCE in the chain (duplicates — e.g.
// a stale rule left beside a fresh one for the same host port). Pure (no exec) so
// it is unit-testable without nft or root. A rule line looks like:
//
//	tcp dport 8741 dnat to 10.100.0.29:8741 # handle 5
func parseDNATChain(listing string) (rules map[string]string, dups map[string]bool) {
	rules = map[string]string{}
	dups = map[string]bool{}
	for _, line := range strings.Split(listing, "\n") {
		f := strings.Fields(line)
		for i := 0; i+5 < len(f); i++ {
			if f[i+1] == "dport" && f[i+3] == "dnat" && f[i+4] == "to" &&
				(f[i] == "tcp" || f[i] == "udp") {
				key := f[i] + "/" + f[i+2]
				if _, seen := rules[key]; seen {
					dups[key] = true
				}
				rules[key] = f[i+5]
				break
			}
		}
	}
	return rules, dups
}

// listChainForwards runs `nft list chain ip <table> <chain>` and parses it. A
// missing table/chain (nft prints "No such file or directory") is reported as an
// EMPTY result with no error — that is genuine "nothing installed yet" drift the
// caller should heal, not a transient read failure. Any other error (nft absent,
// permission denied, malformed output) is returned so the caller can skip the
// tick instead of churning the whole table.
func listChainForwards(chain string) (rules map[string]string, dups map[string]bool, err error) {
	out, err := exec.Command("nft", "list", "chain", "ip", NATTableName, chain).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "No such file or directory") {
			return map[string]string{}, map[string]bool{}, nil
		}
		return nil, nil, fmt.Errorf("nft list chain %s: %s: %w", chain, strings.TrimSpace(string(out)), err)
	}
	rules, dups = parseDNATChain(string(out))
	return rules, dups, nil
}

// CurrentPortForwards returns the published-port DNAT forwards that are FULLY and
// unambiguously installed, as map["<proto>/<hostPort>"] = "<targetIP>:<targetPort>".
// A key is included only if it is present with the SAME target in BOTH the
// prerouting and output chains (AddPortForward writes both) and is NOT duplicated
// in either chain. A key that is missing from a chain, disagrees between chains,
// or is duplicated is deliberately OMITTED so the reconciler treats it as drift
// and re-applies (AddPortForward rewrites both chains and evicts duplicates).
// Returns (nil, err) on a transient nft read error so the caller can skip the
// tick rather than churn the whole table.
func CurrentPortForwards() (map[string]string, error) {
	pre, preDup, err := listChainForwards("prerouting")
	if err != nil {
		return nil, err
	}
	out, outDup, err := listChainForwards("output")
	if err != nil {
		return nil, err
	}
	merged := map[string]string{}
	for k, v := range pre {
		if preDup[k] || outDup[k] {
			continue // ambiguous — force a re-apply
		}
		if ov, ok := out[k]; ok && ov == v {
			merged[k] = v // agrees across both chains, no duplicates
		}
	}
	return merged, nil
}

// parseHostPortKey splits a "proto/hostPort" reconcile key (the format produced
// for CurrentPortForwards / PublishedHostPorts) back into its parts, so the prune
// path can call RemovePortForwardForHostPort. Returns ok=false on a malformed key.
func parseHostPortKey(key string) (proto string, hostPort int, ok bool) {
	slash := strings.IndexByte(key, '/')
	if slash <= 0 {
		return "", 0, false
	}
	p, err := strconv.Atoi(key[slash+1:])
	if err != nil {
		return "", 0, false
	}
	return key[:slash], p, true
}

// PublishedHostPorts returns the set of "<proto>/<hostPort>" keys that have ANY
// DNAT rule in either managed chain, regardless of target or duplication. The
// reconciler uses it to find ORPHAN forwards — host ports still published in
// nftables that no running container claims any more — so they can be pruned.
// Returns (nil, err) on a transient read error.
func PublishedHostPorts() (map[string]bool, error) {
	pre, _, err := listChainForwards("prerouting")
	if err != nil {
		return nil, err
	}
	out, _, err := listChainForwards("output")
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for k := range pre {
		keys[k] = true
	}
	for k := range out {
		keys[k] = true
	}
	return keys, nil
}

// deleteMatchingNATRules walks the prerouting and output chains of the managed
// NAT table and deletes every rule whose listing line satisfies match, keyed by
// the nft handle. Missing chains are skipped.
func deleteMatchingNATRules(match func(line string) bool) error {
	for _, chain := range []string{"prerouting", "output"} {
		out, err := exec.Command("nft", "-a", "list", "chain", "ip", NATTableName, chain).CombinedOutput()
		if err != nil {
			continue // chain may not exist
		}
		for _, handle := range natHandlesToDelete(string(out), match) {
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

// stableLANMac derives a locally-administered unicast MAC from stable
// container identity. LXC otherwise generates a new veth MAC after CT
// recreation, which breaks LAN clients that bind pairing state to MAC address
// (Moonlight/Wolf is the common case).
func stableLANMac(id string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(id)))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
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
func applyLANNetworking(cfg *ContainerConfig, lan LANConfig, vmid int, lanMAC string) bool {
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
	cfg.LANMacAddress = strings.TrimSpace(lanMAC)
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
func DualNICConfig(lanBridge, lanIP, lanGateway, lanMAC, internalIP string) []configItem {
	// net.0 = LAN (primary): routable IP on the physical network.
	items := []configItem{
		{"lxc.net.0.type", "veth"},
		{"lxc.net.0.link", lanBridge},
		{"lxc.net.0.flags", "up"},
	}
	// A stable, locally-administered MAC keeps LAN clients that bind state to
	// the NIC's hardware address (Moonlight/Wolf pairing) working across CT
	// recreation. Applied for both DHCP and static addressing.
	if lanMAC = strings.TrimSpace(lanMAC); lanMAC != "" {
		items = append(items, configItem{"lxc.net.0.hwaddr", lanMAC})
	}
	if lanIP == "dhcp" {
		// DHCP: bring the NIC up with a known name but no static address. The
		// lease itself is obtained by the daemon after start (see maybeLANDHCP) —
		// PVE silently rejects custom lxc.hook.* directives, and the unmanaged gow
		// images ship no DHCP client, so the daemon runs one in the netns.
		items = append(items, configItem{"lxc.net.0.name", "eth0"})
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

// leaseLANDHCP obtains a DHCP lease for the LAN NIC (eth0) from inside the
// container's network namespace, using the host's dhcpcd (the unmanaged gow
// images ship no DHCP client, and PVE silently rejects custom lxc.hook.*
// directives — so the daemon drives the client itself after start).
//
// One-shot (-1): dhcpcd configures the address then exits, leaving no lingering
// host-wide master to collide with the next start (dhcpcd keys its control state
// on the interface name, which repeats as "eth0" across netns). Synchronous and
// bounded (-t), so the lease lands before the workload's later mDNS bind.
// --nohook resolv.conf keeps it from rewriting the host's DNS config.
func leaseLANDHCP(initPID int) error {
	if _, err := exec.LookPath("dhcpcd"); err != nil {
		return fmt.Errorf("dhcpcd not found on host: %w", err)
	}
	out, err := exec.Command("nsenter", "-t", strconv.Itoa(initPID), "-n",
		"dhcpcd", "-1", "-t", "12", "-q", "--nohook", "resolv.conf", "eth0").CombinedOutput()
	if err != nil {
		return fmt.Errorf("dhcpcd: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
