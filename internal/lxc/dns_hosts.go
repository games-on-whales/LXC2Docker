package lxc

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// Container-name DNS for the managed bridge.
//
// LXC2Docker has no embedded DNS resolver, so — like Docker's libnetwork
// sandbox /etc/hosts — the daemon writes name→IP records for every
// container that shares a network into each container's /etc/hosts. A
// dependent (e.g. a service pointed at "postgres") then resolves a peer
// by stable name regardless of which bridge IP it currently holds.
//
// The records are refreshed whenever a peer's IP could have changed
// (just before a container starts, right after one reaches running, and
// on the periodic network reconcile), so a peer that drifted to a new
// address is resolvable again without recreating the dependent — the
// recurring failure mode when callers baked a peer's IP into their env.
//
// Only a daemon-managed block is touched; everything else in the file
// (the image's localhost lines, --add-host entries) is preserved.
const (
	hostsManagedBegin = "# >>> docker-lxc-daemon: container DNS (managed, do not edit) >>>"
	hostsManagedEnd   = "# <<< docker-lxc-daemon: container DNS (managed) <<<"
)

// hostsPeer is one resolvable container: an IP and the names that map to
// it (the container name plus any network aliases).
type hostsPeer struct {
	ip    string
	names []string
}

// canonicalNetwork folds the Docker aliases for "the managed bridge"
// onto its real name so membership comparisons line up. Mirrors the API
// layer's canonicalNetworkName without creating an import cycle.
func canonicalNetwork(name string) string {
	switch strings.TrimSpace(name) {
	case "", "default", "bridge":
		return DefaultNetworkName
	default:
		return name
	}
}

// networkKeys is the set of (canonical) networks a container is attached
// to. A record with no explicit attachments but a managed IP is treated
// as being on the default bridge, matching how the API layer back-fills
// such records.
func networkKeys(rec *store.ContainerRecord) map[string]bool {
	keys := map[string]bool{}
	for name := range rec.Networks {
		keys[canonicalNetwork(name)] = true
	}
	if len(keys) == 0 && rec.IPAddress != "" {
		keys[DefaultNetworkName] = true
	}
	return keys
}

// sharesNetwork reports whether two network key-sets intersect.
func sharesNetwork(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

// containerRootfs returns the host-side rootfs path for a container,
// handling both the raw-LXC and Proxmox (VMID) backends.
func (m *Manager) containerRootfs(rec *store.ContainerRecord) string {
	if rec.VMID > 0 {
		return m.pveRootfsPath(rec.VMID)
	}
	return filepath.Join(m.lxcPath, rec.ID, "rootfs")
}

// peerIPAndAliases picks a container's IP and aliases on the networks it
// shares with mine, falling back to the primary IPAddress.
func peerIPAndAliases(rec *store.ContainerRecord, mine map[string]bool) (string, []string) {
	ip := rec.IPAddress
	var aliases []string
	for name, att := range rec.Networks {
		if !mine[canonicalNetwork(name)] {
			continue
		}
		if att.IPAddress != "" {
			ip = att.IPAddress
		}
		aliases = append(aliases, att.Aliases...)
	}
	return ip, aliases
}

// hostsPeersFor returns the resolvable records for the container rec —
// every container (including rec itself, as Docker does) that shares one
// of rec's networks and has an IP and a name.
func (m *Manager) hostsPeersFor(rec *store.ContainerRecord, all []*store.ContainerRecord) []hostsPeer {
	mine := networkKeys(rec)
	if len(mine) == 0 {
		return nil
	}
	var peers []hostsPeer
	for _, other := range all {
		if other.Name == "" {
			continue
		}
		if !sharesNetwork(mine, networkKeys(other)) {
			continue
		}
		ip, aliases := peerIPAndAliases(other, mine)
		if ip == "" {
			continue
		}
		names := dedupeStrings(append([]string{other.Name}, aliases...))
		peers = append(peers, hostsPeer{ip: ip, names: names})
	}
	return peers
}

// renderManagedHostsBlock renders the records as /etc/hosts lines,
// sorted by first name for deterministic, churn-free output. Returns ""
// when there are no records.
func renderManagedHostsBlock(peers []hostsPeer) string {
	if len(peers) == 0 {
		return ""
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].names[0] < peers[j].names[0] })
	var b strings.Builder
	for _, p := range peers {
		fmt.Fprintf(&b, "%s\t%s\n", p.ip, strings.Join(p.names, " "))
	}
	return b.String()
}

// stripManagedHostsBlock removes a previously written managed block
// (markers included) from an /etc/hosts body, leaving all other content
// — image localhost lines, --add-host entries — untouched.
func stripManagedHostsBlock(body string) string {
	begin := strings.Index(body, hostsManagedBegin)
	if begin < 0 {
		return body
	}
	end := strings.Index(body, hostsManagedEnd)
	if end < 0 || end < begin {
		// Truncated/corrupt block: drop from the begin marker on.
		return strings.TrimRight(body[:begin], "\n") + "\n"
	}
	end += len(hostsManagedEnd)
	if nl := strings.IndexByte(body[end:], '\n'); nl >= 0 {
		end += nl + 1
	}
	out := body[:begin] + body[end:]
	return out
}

// composeHostsFile splices a freshly rendered managed block into an
// existing /etc/hosts body, replacing any prior managed block.
func composeHostsFile(existing, block string) string {
	base := stripManagedHostsBlock(existing)
	if block == "" {
		return base
	}
	if base != "" && !strings.HasSuffix(base, "\n") {
		base += "\n"
	}
	return base + hostsManagedBegin + "\n" + block + hostsManagedEnd + "\n"
}

// writeContainerHosts rewrites the managed DNS block in one container's
// /etc/hosts from the current store state. A no-op when the content is
// unchanged or the container's rootfs isn't materialised yet. The write
// is atomic (temp + rename) so a container never reads a half-written
// file.
func (m *Manager) writeContainerHosts(rec *store.ContainerRecord, all []*store.ContainerRecord) error {
	etcDir := filepath.Join(m.containerRootfs(rec), "etc")
	if fi, err := os.Stat(etcDir); err != nil || !fi.IsDir() {
		return nil // rootfs not prepared (or container gone) — nothing to do
	}
	hostsPath := filepath.Join(etcDir, "hosts")

	existing, _ := os.ReadFile(hostsPath)
	block := renderManagedHostsBlock(m.hostsPeersFor(rec, all))
	out := composeHostsFile(string(existing), block)
	if out == string(existing) {
		return nil
	}

	tmp, err := os.CreateTemp(etcDir, ".hosts-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(out); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmpName, 0o644)
	return os.Rename(tmpName, hostsPath)
}

// syncHosts rewrites the managed DNS block in every container's
// /etc/hosts. Idempotent and cheap (a handful of small files, written
// only when changed); safe to call on the network-reconcile ticker and
// after any start/stop.
func (m *Manager) syncHosts() {
	all := m.store.ListContainers()
	for _, rec := range all {
		if err := m.writeContainerHosts(rec, all); err != nil {
			log.Printf("container DNS: write /etc/hosts for %s: %v", shortID(rec.ID), err)
		}
	}
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
