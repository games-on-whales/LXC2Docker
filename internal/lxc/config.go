package lxc

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// configItem is a key/value pair written to an LXC config file.
type configItem struct {
	key   string
	value string
}

// ContainerConfig holds the Docker-layer configuration fields that we
// translate into LXC config items. This is populated from the Docker API
// container-create request body.
type ContainerConfig struct {
	Entrypoint        []string
	Cmd               []string
	Env               []string
	Mounts            []MountSpec  // bind mounts
	Devices           []DeviceSpec // host devices to expose
	DeviceCgroupRules []string     // e.g. ["c 13:* rwm"]
	NetworkMode       string       // "host" or "" (bridge)
	IpcMode           string       // "host" or "" (private)
	UTSMode           string       // "host" or "" (private)
	PidMode           string       // "host" or "" (private)
	MemoryBytes       int64        // 0 = unlimited
	CPUShares         int64        // 0 = unlimited (relative weight)
	CPUQuota          int64        // microseconds per 100ms period, 0 = unlimited
	NanoCPUs          int64        // Docker's CPU limit in units of 1e-9 CPU; 1.5 CPU = 1.5e9
	CpusetCpus        string       // Docker's --cpuset-cpus (e.g. "0-3", "0,2")
	CpusetMems        string       // Docker's --cpuset-mems (e.g. "0")
	PidsLimit         int64        // Maximum PIDs in the container (0 = unlimited)
	Ulimits           []Ulimit     // Docker-style ulimits (lxc.prlimit.<name>)
	ShmSize           int64        // /dev/shm tmpfs size in bytes (0 = kernel default)
	BlkioWeight       uint16       // Block I/O weight (10-1000, mapped to io.weight)
	WorkingDir        string       // container cwd; maps to lxc.init.cwd
	// Security. Privileged grants full capabilities + unrestricted device
	// access; equivalent to Docker's --privileged. CapAdd/CapDrop extend
	// or restrict the default set when not privileged.
	Privileged  bool
	CapAdd      []string // Docker-style names e.g. "NET_ADMIN"; CAP_ prefix optional
	CapDrop     []string
	SecurityOpt []string // Docker's --security-opt; e.g. "no-new-privileges:true"
	// Sysctls maps kernel parameter name → value. Written as
	// lxc.sysctl.<key> = <val>. LXC only applies the subset that's
	// namespaced (net.*, kernel.*); host-wide keys are rejected at start.
	Sysctls map[string]string
	// Tmpfs maps in-container destination path → mount options
	// (e.g. "/run" → "rw,nosuid,size=65536k"). Empty options use
	// reasonable Docker-compatible defaults.
	Tmpfs map[string]string
	// ExtraHosts is a list of "name:ip" pairs appended to /etc/hosts in
	// the container rootfs at create time. Docker's --add-host.
	ExtraHosts []string
	DNS        []string
	DNSSearch  []string
	DNSOptions []string
	// LogFile is where the container console output is written.
	// Set automatically by the manager.
	LogFile string
	// SocketLinks records symlinks to create in prepareRootfs for socket
	// bind mounts. Maps in-container destination path → symlink target
	// (absolute in-container path inside the directory mount). Populated
	// automatically by buildItems / buildPVEItems when they replace a
	// socket file bind-mount with a directory mount.
	SocketLinks map[string]string
	// ProxmoxCT requests that this container be created as a Proxmox CT
	// (visible in the Proxmox web UI). When false and PVE mode is active,
	// the container is created as an ephemeral raw-LXC container with a
	// ZFS-cloned rootfs — invisible to Proxmox but still on the PVE storage.
	ProxmoxCT bool
	// LAN requests a second NIC on a LAN bridge. Only effective for
	// Proxmox CTs. The bridge is selected by Bridge (or daemon default
	// if Bridge is empty); the IP is derived from VMID and the bridge spec.
	LAN bool
	// Bridge names which configured LAN bridge to attach when LAN is true.
	// Empty means use the daemon's default bridge. Set from "dld.bridge"
	// label.
	Bridge string
	// Storage is the per-container PVE storage override for rootfs (only
	// honored for permanent CTs; ephemeral containers must use the
	// template's storage because ZFS clones are pool-local). Empty means
	// the daemon default. Set from "dld.storage" label.
	Storage string
	// ISOs are read-only ISO files bind-mounted into the container.
	// The daemon resolves each Storage:VolumeID via `pvesm path` and binds
	// the resulting file to Destination. Set from "dld.iso" label
	// (comma-separated list of "storage:volid[:dest]").
	ISOs []ISOMount
	// LANBridge, LANIP, LANGateway are filled in by the manager (not the
	// API layer) once Bridge has been resolved against LANConfig.
	LANBridge  string
	LANIP      string
	LANGateway string
}

// ISOMount describes one read-only ISO bind-mount.
type ISOMount struct {
	Storage     string // PVE storage name (e.g. "isos")
	VolumeID    string // path within storage (e.g. "iso/Win11.iso")
	Destination string // path inside the container (e.g. "/mnt/win11.iso")
}

// Ulimit describes a single rlimit pair. Name matches Docker's convention
// (nofile, nproc, stack, etc.).
type Ulimit struct {
	Name string
	Soft int64
	Hard int64
}

// MountSpec describes a single bind mount.
type MountSpec struct {
	Source      string
	Destination string
	ReadOnly    bool
}

// DeviceSpec describes a host device to expose inside the container.
type DeviceSpec struct {
	PathOnHost      string
	PathInContainer string
}

// EphemeralMarker is a comment line written into the on-disk LXC config for
// every ephemeral container created by this daemon. The GC requires this
// marker to be present before destroying a container — it is the positive
// identification that a container was created as ephemeral by this daemon.
// Permanent Proxmox CTs (writePVEConfig) never receive this marker.
const EphemeralMarker = "# docker-lxc-daemon: ephemeral"

// ManagedTag is the Proxmox CT tag the daemon writes onto every permanent
// container it creates. It also serves as the opt-in adoption marker:
// any pre-existing PVE CT carrying this tag (added by the operator via
// the PVE UI or `pct set <vmid> --tags ...`) is surfaced to Docker
// clients via this daemon as if the daemon owned it.
//
// Untagged PVE CTs are invisible to the daemon — listContainers does not
// return them and lifecycle calls against their VMIDs are rejected.
// Removing the tag from a CT releases it from daemon management.
const ManagedTag = "dld-managed"

// rewriteConfig reads the cloned LXC config file, strips problematic lines
// inherited from the download template (userns, apparmor, duplicate network),
// and appends the daemon-managed config items. This is more reliable than
// the go-lxc SetConfigItem API because lxc.include directives are processed
// at container start time and can override in-memory changes.
//
// If ephemeral is true, writes EphemeralMarker into the config so the GC
// can positively identify this container as GC-eligible.
func rewriteConfig(path string, cfg *ContainerConfig, ip, containerName string, ephemeral bool) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	var kept []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.Contains(trimmed, "userns.conf"):
			continue
		case strings.HasPrefix(trimmed, "lxc.apparmor.profile"):
			continue
		case strings.HasPrefix(trimmed, "lxc.apparmor.allow_nesting"):
			continue
		case strings.HasPrefix(trimmed, "lxc.net."):
			continue
		case strings.HasPrefix(trimmed, "lxc.idmap"):
			continue
		case strings.HasPrefix(trimmed, "lxc.id_map"):
			continue
		}

		kept = append(kept, line)
	}
	f.Close()
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan config: %w", err)
	}

	items := append([]configItem{
		{"lxc.apparmor.profile", "unconfined"},
		// Override common.conf's cgroup:mixed which fails on Proxmox cgroup v2.
		// An empty value clears the inherited list; then we set what we need.
		{"lxc.mount.auto", ""},
		{"lxc.mount.auto", "proc:mixed sys:mixed"},
	}, buildItems(cfg, ip)...)
	// Note: buildItems may populate cfg.SocketLinks (for socket bind mounts).

	// Resolve mount entry destinations against the container's rootfs so that
	// any symlinks (e.g. /var/run → /run) are followed. LXC rejects mount
	// entries whose destination paths traverse symlinks in the rootfs.
	// Parse the actual rootfs path from the config (lxc.rootfs.path = dir:/path)
	// rather than assuming config_dir/rootfs — ephemeral ZFS clones use a
	// separate mountpoint.
	rootfs := filepath.Join(filepath.Dir(path), "rootfs")
	for _, line := range kept {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "lxc.rootfs.path") {
			parts := strings.SplitN(trimmed, "=", 2)
			if len(parts) == 2 {
				val := strings.TrimSpace(parts[1])
				// Strip "dir:" prefix if present.
				val = strings.TrimPrefix(val, "dir:")
				if val != "" {
					rootfs = val
				}
			}
		}
	}
	for i, item := range items {
		if item.key == "lxc.mount.entry" {
			items[i].value = resolveMountDest(rootfs, item.value)
		}
	}

	out, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	defer out.Close()

	w := bufio.NewWriter(out)
	for _, line := range kept {
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w, "\n# docker-lxc-daemon managed config")
	if ephemeral {
		fmt.Fprintln(w, EphemeralMarker)
	}
	for _, item := range items {
		fmt.Fprintf(w, "%s = %s\n", item.key, item.value)
	}
	return w.Flush()
}

// resolveMountDest rewrites the destination field of an lxc.mount.entry so
// that it does not traverse symlinks in the container rootfs. LXC's
// open_without_symlink check rejects destinations that go through symlinks
// (e.g. var/run → /run in modern Ubuntu-based images).
func resolveMountDest(rootfs, entry string) string {
	fields := strings.Fields(entry)
	if len(fields) < 6 {
		return entry
	}
	destRel := fields[1] // relative to rootfs (no leading slash)

	// Walk each path component, following symlinks within the rootfs.
	parts := strings.Split(filepath.Clean("/"+destRel), "/")
	current := ""
	for _, part := range parts {
		if part == "" {
			continue
		}
		next := filepath.Join(current, part)
		fullPath := filepath.Join(rootfs, next)
		if link, err := os.Readlink(fullPath); err == nil {
			// This component is a symlink — resolve it.
			if filepath.IsAbs(link) {
				current = link
			} else {
				current = filepath.Join(filepath.Dir(next), link)
			}
		} else {
			current = next
		}
	}
	current = strings.TrimPrefix(current, "/")
	fields[1] = current
	return strings.Join(fields, " ")
}

// resolveInRootfs resolves a container-absolute path by following symlinks
// within the rootfs. Returns the resolved path (relative, no leading slash).
func resolveInRootfs(rootfs, containerPath string) (string, error) {
	parts := strings.Split(filepath.Clean(containerPath), "/")
	current := ""
	for _, part := range parts {
		if part == "" {
			continue
		}
		next := filepath.Join(current, part)
		fullPath := filepath.Join(rootfs, next)
		if link, err := os.Readlink(fullPath); err == nil {
			if filepath.IsAbs(link) {
				current = strings.TrimPrefix(link, "/")
			} else {
				current = filepath.Join(filepath.Dir(next), link)
			}
		} else {
			current = next
		}
	}
	return current, nil
}

func buildItems(cfg *ContainerConfig, ip string) []configItem {
	var items []configItem

	// Docker-compatible default mounts: /dev/shm (shared memory) is required
	// by most graphical apps (Wayland/wlroots), IPC, and many libraries.
	items = append(items, configItem{"lxc.mount.entry", shmMountEntry(cfg.ShmSize)})

	// Network configuration.
	if cfg.LANBridge != "" {
		items = append(items, DualNICConfig(cfg.LANBridge, cfg.LANIP, cfg.LANGateway, ip)...)
	} else if cfg.NetworkMode == "host" {
		// Handled below via lxc.namespace.clone.
	} else {
		items = append(items, NetworkConfig(ip)...)
	}

	// Namespace sharing: lxc.namespace.clone lists which namespaces to
	// CREATE (clone). Omitting a namespace means the container shares the
	// host's instance. We only set this when at least one namespace should
	// be shared (Docker's NetworkMode/IpcMode/UTSMode/PidMode "host").
	if cfg.NetworkMode == "host" || cfg.IpcMode == "host" || cfg.UTSMode == "host" || cfg.PidMode == "host" {
		ns := []string{"mnt"}
		if cfg.NetworkMode != "host" {
			ns = append(ns, "net")
		}
		if cfg.IpcMode != "host" {
			ns = append(ns, "ipc")
		}
		if cfg.UTSMode != "host" {
			ns = append(ns, "uts")
		}
		if cfg.PidMode != "host" {
			ns = append(ns, "pid")
		}
		items = append(items, configItem{"lxc.namespace.clone", strings.Join(ns, " ")})
	}

	// Environment variables — reject newlines to prevent config injection.
	for _, e := range cfg.Env {
		if strings.ContainsAny(e, "\n\r") {
			continue
		}
		items = append(items, configItem{"lxc.environment", e})
	}

	// Entrypoint + cmd: combined into lxc.init.cmd.
	// LXC runs this as the container's PID 1.
	if combined := combinedCmd(cfg.Entrypoint, cfg.Cmd); combined != "" {
		items = append(items, configItem{"lxc.init.cmd", combined})
	}

	// Working directory: maps to lxc.init.cwd (Docker's WorkingDir / OCI WORKDIR).
	if cfg.WorkingDir != "" {
		items = append(items, configItem{"lxc.init.cwd", cfg.WorkingDir})
	}

	// Bind mounts
	for _, m := range cfg.Mounts {
		// Resolve symlinks in the source so LXC gets the real path.
		// LXC rejects bind-mounting through symlinks (e.g. /var/run → /run).
		source := m.Source
		if real, err := filepath.EvalSymlinks(source); err == nil {
			source = real
		}

		// Unix socket special handling: mount the parent directory instead
		// of the socket file. File bind-mounts follow inodes, so if the
		// socket is recreated (e.g. daemon restart), a file mount goes
		// stale. A directory mount sees the new file automatically.
		if fi, err := os.Stat(source); err == nil && fi.Mode()&os.ModeSocket != 0 {
			items = appendSocketMount(items, cfg, source, m)
			continue
		}

		// Detect whether source is a directory or a file so we use
		// the correct create= option. LXC will refuse to mount a file onto a
		// directory placeholder (or vice-versa).
		createOpt := "create=dir"
		if fi, err := os.Stat(source); err == nil && !fi.IsDir() {
			createOpt = "create=file"
		}
		opts := "bind," + createOpt
		if m.ReadOnly {
			opts += ",ro"
		}
		// lxc.mount.entry format (fstab-style, space-delimited):
		//   <source> <dest-relative-to-rootfs> <fs-type> <options> 0 0
		// Spaces in paths must be escaped as \040 (octal, like /etc/fstab).
		dest := strings.TrimPrefix(m.Destination, "/")
		escapedSource := strings.ReplaceAll(source, " ", `\040`)
		escapedDest := strings.ReplaceAll(dest, " ", `\040`)
		entry := fmt.Sprintf("%s %s none %s 0 0", escapedSource, escapedDest, opts)
		items = append(items, configItem{"lxc.mount.entry", entry})
	}

	// Devices
	for _, d := range cfg.Devices {
		dest := d.PathInContainer
		if dest == "" {
			dest = d.PathOnHost
		}
		destRel := strings.TrimPrefix(dest, "/")

		// Resolve symlinks in the source path.
		hostPath := d.PathOnHost
		if real, err := filepath.EvalSymlinks(hostPath); err == nil {
			hostPath = real
		}

		// Detect whether the source is a directory or a device node.
		fi, err := os.Stat(hostPath)
		isDir := err == nil && fi.IsDir()

		if isDir {
			// For device directories (e.g. /dev/dri), bind-mount the whole
			// directory and add cgroup allow rules for each device node inside.
			items = append(items, configItem{
				"lxc.mount.entry",
				fmt.Sprintf("%s %s none bind,create=dir 0 0", hostPath, destRel),
			})
			// Scan directory for device nodes and allow each one.
			if entries, err := os.ReadDir(hostPath); err == nil {
				for _, entry := range entries {
					if entry.IsDir() {
						continue
					}
					entryPath := filepath.Join(hostPath, entry.Name())
					if rule := deviceCgroupEntry(entryPath); rule != "" {
						items = append(items, configItem{
							"lxc.cgroup2.devices.allow", rule,
						})
					}
				}
			}
		} else {
			// For individual device nodes, add a precise cgroup allow rule.
			if rule := deviceCgroupEntry(hostPath); rule != "" {
				items = append(items, configItem{
					"lxc.cgroup2.devices.allow", rule,
				})
			}
			items = append(items, configItem{
				"lxc.mount.entry",
				fmt.Sprintf("%s %s none bind,create=file 0 0", hostPath, destRel),
			})
		}
	}

	// Device cgroup rules (e.g. "c 13:* rwm")
	for _, rule := range cfg.DeviceCgroupRules {
		items = append(items, configItem{"lxc.cgroup2.devices.allow", rule})
	}
	// Auto-mount host device directories for wildcard cgroup rules. In
	// Docker, cgroup rules + MKNOD cap are sufficient because containers
	// share the host's devtmpfs (or Docker creates device nodes). In LXC
	// the device files must physically exist in the container's /dev.
	items = append(items, autoMountDeviceDirs(cfg.DeviceCgroupRules)...)

	// Memory limit
	if cfg.MemoryBytes > 0 {
		items = append(items, configItem{
			"lxc.cgroup2.memory.max",
			fmt.Sprintf("%d", cfg.MemoryBytes),
		})
	}

	// CPU
	if cfg.CPUShares > 0 {
		items = append(items, configItem{
			"lxc.cgroup2.cpu.weight",
			fmt.Sprintf("%d", cpuSharesToWeight(cfg.CPUShares)),
		})
	}
	if cfg.CPUQuota > 0 {
		// Docker CPUQuota is in microseconds; LXC cpu.max is "quota period"
		// where period defaults to 100000 µs.
		items = append(items, configItem{
			"lxc.cgroup2.cpu.max",
			fmt.Sprintf("%d 100000", cfg.CPUQuota),
		})
	} else if cfg.NanoCPUs > 0 {
		// NanoCPUs (Docker's --cpus flag) — 1 CPU = 1e9 NanoCPUs. Convert
		// to cgroup v2 quota µs at the default 100 ms period. Used only
		// when an explicit CPUQuota hasn't already pinned the value.
		quota := cfg.NanoCPUs * 100000 / 1_000_000_000
		if quota < 1000 {
			quota = 1000
		}
		items = append(items, configItem{
			"lxc.cgroup2.cpu.max",
			fmt.Sprintf("%d 100000", quota),
		})
	}
	if cfg.CpusetCpus != "" {
		items = append(items, configItem{"lxc.cgroup2.cpuset.cpus", cfg.CpusetCpus})
	}
	if cfg.CpusetMems != "" {
		items = append(items, configItem{"lxc.cgroup2.cpuset.mems", cfg.CpusetMems})
	}
	if cfg.PidsLimit > 0 {
		items = append(items, configItem{"lxc.cgroup2.pids.max", fmt.Sprintf("%d", cfg.PidsLimit)})
	}
	if cfg.BlkioWeight > 0 {
		items = append(items, configItem{"lxc.cgroup2.io.weight", fmt.Sprintf("default %d", cfg.BlkioWeight)})
	}

	// Privileged + capability handling. Docker's --privileged maps to two
	// LXC side-effects: all capabilities are kept (we clear lxc.cap.drop)
	// and unrestricted device access is allowed. Non-privileged CapAdd /
	// CapDrop translate to lxc.cap.keep / lxc.cap.drop entries.
	items = append(items, capabilityItems(cfg)...)
	items = append(items, securityOptItems(cfg)...)

	// Sysctls and Tmpfs: translated directly to LXC directives. Docker's
	// --sysctl / --tmpfs forms both map cleanly without extra runtime work.
	items = append(items, sysctlItems(cfg)...)
	items = append(items, tmpfsItems(cfg)...)
	items = append(items, ulimitItems(cfg)...)

	// Console log so we can serve it via the logs API
	if cfg.LogFile != "" {
		items = append(items, configItem{"lxc.console.logfile", cfg.LogFile})
	}

	return items
}

// capabilityItems translates Docker's Privileged / CapAdd / CapDrop into LXC
// config lines. Privileged wins — when set we clear all drops and grant
// full device cgroup access. Otherwise CapDrop/CapAdd produce matching
// lxc.cap.drop / lxc.cap.keep entries, one capability per line.
func capabilityItems(cfg *ContainerConfig) []configItem {
	var items []configItem
	if cfg.Privileged {
		// Clear any inherited drops from common.conf and allow all devices.
		items = append(items,
			configItem{"lxc.cap.drop", ""},
			configItem{"lxc.cgroup2.devices.allow", "a"},
		)
		return items
	}
	for _, c := range cfg.CapDrop {
		items = append(items, configItem{"lxc.cap.drop", normalizeCap(c)})
	}
	for _, c := range cfg.CapAdd {
		items = append(items, configItem{"lxc.cap.keep", normalizeCap(c)})
	}
	return items
}

// normalizeCap converts Docker's capability name ("NET_ADMIN", "CAP_NET_ADMIN")
// to LXC's form (lowercase, no CAP_ prefix).
func normalizeCap(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimPrefix(name, "cap_")
	return name
}

// securityOptItems translates Docker --security-opt entries into LXC
// directives. Currently only "no-new-privileges[:true|false]" is wired
// through; other opts (seccomp profile overrides, apparmor) would require
// custom profile plumbing outside the scope of this translation layer.
func securityOptItems(cfg *ContainerConfig) []configItem {
	var items []configItem
	for _, opt := range cfg.SecurityOpt {
		opt = strings.TrimSpace(opt)
		key, val, _ := strings.Cut(opt, "=")
		if val == "" {
			key, val, _ = strings.Cut(opt, ":")
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "no-new-privileges":
			v := strings.ToLower(strings.TrimSpace(val))
			if v == "" || v == "true" || v == "1" {
				items = append(items, configItem{"lxc.no_new_privs", "1"})
			}
		}
	}
	return items
}

// sysctlItems emits one lxc.sysctl.<key> = <value> per configured sysctl.
// LXC applies these in the container's namespaces at start; keys it can't
// set (non-namespaced, like kernel.pid_max) cause the container to fail
// fast with a clear error, which matches Docker's behavior.
func sysctlItems(cfg *ContainerConfig) []configItem {
	if len(cfg.Sysctls) == 0 {
		return nil
	}
	items := make([]configItem, 0, len(cfg.Sysctls))
	for k, v := range cfg.Sysctls {
		// Reject injection via newlines/equals in the key — LXC would
		// otherwise parse a value as a second config directive.
		if strings.ContainsAny(k, "\n\r=") || strings.ContainsAny(v, "\n\r") {
			continue
		}
		items = append(items, configItem{"lxc.sysctl." + k, v})
	}
	return items
}

func shmMountEntry(size int64) string {
	opts := "rw,nosuid,nodev,create=dir"
	if size > 0 {
		opts += fmt.Sprintf(",size=%d", size)
	}
	return "tmpfs dev/shm tmpfs " + opts + " 0 0"
}

// ulimitItems maps HostConfig.Ulimits onto lxc.prlimit.<name> directives.
// LXC's format is `soft[:hard]`; when Hard equals Soft or is zero we emit
// just the soft value.
func ulimitItems(cfg *ContainerConfig) []configItem {
	if len(cfg.Ulimits) == 0 {
		return nil
	}
	items := make([]configItem, 0, len(cfg.Ulimits))
	for _, u := range cfg.Ulimits {
		name := strings.ToLower(strings.TrimSpace(u.Name))
		if name == "" || strings.ContainsAny(name, "\r\n=") {
			continue
		}
		val := formatUlimitValue(u.Soft, u.Hard)
		items = append(items, configItem{"lxc.prlimit." + name, val})
	}
	return items
}

func formatUlimitValue(soft, hard int64) string {
	softStr := "unlimited"
	if soft >= 0 {
		softStr = fmt.Sprintf("%d", soft)
	}
	if hard == 0 || hard == soft {
		return softStr
	}
	hardStr := "unlimited"
	if hard > 0 {
		hardStr = fmt.Sprintf("%d", hard)
	}
	return softStr + ":" + hardStr
}

// tmpfsItems emits one lxc.mount.entry per requested tmpfs. Docker's
// HostConfig.Tmpfs value is a mount option string (e.g. "size=64m,noexec");
// LXC's fstab-style entry wants flags in the 4th column. When the caller
// gave no options we default to the same set Docker uses
// (rw,nosuid,nodev,noexec).
func tmpfsItems(cfg *ContainerConfig) []configItem {
	if len(cfg.Tmpfs) == 0 {
		return nil
	}
	items := make([]configItem, 0, len(cfg.Tmpfs))
	for dest, options := range cfg.Tmpfs {
		if options == "" {
			options = "rw,nosuid,nodev,noexec"
		}
		// The LXC entry is fstab-style. `create=dir` makes LXC mkdir the
		// target if it doesn't exist, matching Docker's behavior for
		// paths that aren't in the image.
		rel := strings.TrimPrefix(dest, "/")
		entry := fmt.Sprintf("tmpfs %s tmpfs %s,create=dir 0 0", rel, options)
		items = append(items, configItem{"lxc.mount.entry", entry})
	}
	return items
}

// combinedCmd merges entrypoint and cmd the same way Docker does.
func combinedCmd(entrypoint, cmd []string) string {
	var parts []string
	parts = append(parts, entrypoint...)
	parts = append(parts, cmd...)
	if len(parts) == 0 {
		return ""
	}
	// LXC splits lxc.init.cmd on spaces. Quote any argument that contains
	// spaces so commands like `/bin/sh -c "nginx -g 'daemon off;'"` are
	// passed correctly.
	var quoted []string
	for _, p := range parts {
		if strings.Contains(p, " ") {
			quoted = append(quoted, `"`+p+`"`)
		} else {
			quoted = append(quoted, p)
		}
	}
	return strings.Join(quoted, " ")
}

// cpuSharesToWeight converts Docker's legacy CPU shares (1–1024) to cgroup v2
// weight (1–10000). Docker default is 1024 → weight 100.
func cpuSharesToWeight(shares int64) int64 {
	if shares <= 0 {
		return 100
	}
	w := (shares * 10000) / 1024
	if w < 1 {
		return 1
	}
	if w > 10000 {
		return 10000
	}
	return w
}

// appendSocketMount replaces a file bind-mount of a Unix socket with a
// directory bind-mount of the socket's parent directory. This survives
// socket recreation (e.g. daemon restart) because directory mounts see
// new files at the same path. A symlink from the original destination to
// the socket inside the directory mount is recorded in cfg.SocketLinks
// for prepareRootfs to create.
func appendSocketMount(items []configItem, cfg *ContainerConfig, source string, m MountSpec) []configItem {
	// Ensure the socket is world-accessible. In Docker, all containers share
	// the host UID namespace so file permissions on sockets are moot. In LXC
	// each container has its own view, so we must explicitly allow all UIDs
	// to connect() to shared sockets (e.g. Wayland, PulseAudio).
	os.Chmod(source, 0o777)

	parentDir := filepath.Dir(source)
	socketName := filepath.Base(source)

	// Mount the parent directory at a hidden location in the container.
	// Use a path derived from the parent dir name to avoid collisions.
	dirName := filepath.Base(parentDir)
	mountDest := ".socket-dirs/" + dirName

	// Only add the directory mount entry once per parent directory.
	alreadyMounted := false
	escapedDest := strings.ReplaceAll(mountDest, " ", `\040`)
	for _, item := range items {
		if item.key == "lxc.mount.entry" && strings.Contains(item.value, " "+escapedDest+" ") {
			alreadyMounted = true
			break
		}
	}
	if !alreadyMounted {
		escapedParent := strings.ReplaceAll(parentDir, " ", `\040`)
		entry := fmt.Sprintf("%s %s none bind,create=dir 0 0", escapedParent, escapedDest)
		items = append(items, configItem{"lxc.mount.entry", entry})
	}

	// Record symlink for prepareRootfs: destination → socket in mounted dir.
	if cfg.SocketLinks == nil {
		cfg.SocketLinks = make(map[string]string)
	}
	cfg.SocketLinks[m.Destination] = "/" + mountDest + "/" + socketName

	return items
}

// autoMountDeviceDirs inspects wildcard cgroup rules (like "c 13:* rwm") and
// bind-mounts the corresponding host device directories so device nodes are
// visible inside the container. In Docker, cgroup rules + MKNOD cap suffice
// because Docker populates /dev from the host; LXC containers have their own
// /dev from the rootfs, so the files must be explicitly mounted.
func autoMountDeviceDirs(rules []string) []configItem {
	// Map well-known device major numbers to host directories.
	majorDirMap := map[string]string{
		"13": "/dev/input", // evdev input devices (keyboard, mouse, gamepad)
	}

	var items []configItem
	mounted := make(map[string]bool)
	for _, rule := range rules {
		fields := strings.Fields(rule) // e.g. ["c", "13:*", "rwm"]
		if len(fields) < 2 {
			continue
		}
		majMin := strings.SplitN(fields[1], ":", 2)
		if len(majMin) != 2 || majMin[1] != "*" {
			continue
		}
		dir, ok := majorDirMap[majMin[0]]
		if !ok || mounted[dir] {
			continue
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		mounted[dir] = true
		destRel := strings.TrimPrefix(dir, "/")
		items = append(items, configItem{
			"lxc.mount.entry",
			fmt.Sprintf("%s %s none bind,create=dir 0 0", dir, destRel),
		})
		// Add per-device cgroup rules for each node in the directory.
		if entries, err := os.ReadDir(dir); err == nil {
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				if rule := deviceCgroupEntry(filepath.Join(dir, entry.Name())); rule != "" {
					items = append(items, configItem{"lxc.cgroup2.devices.allow", rule})
				}
			}
		}
	}
	return items
}

// deviceCgroupEntry returns a cgroup2 device allow rule for a device path.
// Returns "" if the path is not a device node (e.g. a regular file or directory).
// We use "rwm" (read/write/mknod) for all devices passed through.
func deviceCgroupEntry(path string) string {
	major, minor := deviceNumbers(path)
	if major < 0 || (major == 0 && minor == 0) {
		return "" // not a device node — skip
	}
	return fmt.Sprintf("c %d:%d rwm", major, minor)
}

// deviceNumbers returns the major/minor numbers for a device file.
// Returns -1,-1 on error.
func deviceNumbers(path string) (int, int) {
	var stat syscallStat
	if err := syscallStatDevice(path, &stat); err != nil {
		return -1, -1
	}
	return int(stat.major), int(stat.minor)
}

// writePVEConfig writes a Proxmox CT config to /etc/pve/lxc/<vmid>.conf.
// It uses Proxmox-native syntax for core options and raw lxc.* pass-through
// for everything else. The rootfsSpec should be like "large:subvol-260-disk-0,size=4G".
func writePVEConfig(vmid int, hostname, rootfsSpec, rootfsPath string, cfg *ContainerConfig, ip string) error {
	path := fmt.Sprintf("/etc/pve/lxc/%d.conf", vmid)

	var lines []string
	lines = append(lines, "arch: amd64")
	if hostname != "" {
		lines = append(lines, fmt.Sprintf("hostname: %s", hostname))
	}
	if cfg.MemoryBytes > 0 {
		lines = append(lines, fmt.Sprintf("memory: %d", cfg.MemoryBytes/(1024*1024)))
	} else {
		lines = append(lines, "memory: 4096")
	}
	lines = append(lines, "ostype: unmanaged")
	lines = append(lines, fmt.Sprintf("rootfs: %s", rootfsSpec))
	lines = append(lines, "unprivileged: 0")
	// Mark the CT as daemon-managed so listContainers / Portainer surface
	// it. Removing this tag (via PVE UI or `pct set --tags ...`) releases
	// the CT from daemon management.
	lines = append(lines, "tags: "+ManagedTag)

	// Raw lxc.* pass-through items (including network config).
	items := buildPVEItems(cfg, ip)

	// Resolve mount entry destinations against the container rootfs so
	// symlinks (e.g. /var/run → /run) are followed. LXC rejects mount
	// entries whose destination traverses symlinks.
	for i, item := range items {
		if item.key == "lxc.mount.entry" {
			items[i].value = resolveMountDest(rootfsPath, item.value)
		}
	}

	for _, item := range items {
		lines = append(lines, fmt.Sprintf("%s: %s", item.key, item.value))
	}

	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// buildPVEItems returns the lxc.* config items for a Proxmox CT config.
// Uses raw lxc.* directives for all settings including networking, since
// Proxmox-native net0: doesn't reliably configure interfaces in unmanaged
// OS-type containers.
func buildPVEItems(cfg *ContainerConfig, ip string) []configItem {
	var items []configItem

	items = append(items, configItem{"lxc.apparmor.profile", "unconfined"})
	items = append(items, configItem{"lxc.mount.auto", ""})
	items = append(items, configItem{"lxc.mount.auto", "proc:mixed sys:mixed"})

	// /dev/shm
	items = append(items, configItem{"lxc.mount.entry", shmMountEntry(cfg.ShmSize)})

	// Network configuration.
	if cfg.LANBridge != "" {
		items = append(items, DualNICConfig(cfg.LANBridge, cfg.LANIP, cfg.LANGateway, ip)...)
	} else if cfg.NetworkMode == "host" {
		// Handled below via lxc.namespace.clone.
	} else {
		items = append(items, NetworkConfig(ip)...)
	}

	// Namespace sharing (see buildItems for explanation).
	if cfg.NetworkMode == "host" || cfg.IpcMode == "host" {
		ns := []string{"mnt", "pid", "uts"}
		if cfg.NetworkMode != "host" {
			ns = append(ns, "net")
		}
		if cfg.IpcMode != "host" {
			ns = append(ns, "ipc")
		}
		items = append(items, configItem{"lxc.namespace.clone", strings.Join(ns, " ")})
	}

	// Environment variables.
	for _, e := range cfg.Env {
		if strings.ContainsAny(e, "\n\r") {
			continue
		}
		items = append(items, configItem{"lxc.environment", e})
	}

	// Init command.
	if combined := combinedCmd(cfg.Entrypoint, cfg.Cmd); combined != "" {
		items = append(items, configItem{"lxc.init.cmd", combined})
	}

	// Working directory.
	if cfg.WorkingDir != "" {
		items = append(items, configItem{"lxc.init.cwd", cfg.WorkingDir})
	}

	// Bind mounts — use raw lxc.mount.entry (works in Proxmox configs).
	for _, m := range cfg.Mounts {
		source := m.Source
		if real, err := filepath.EvalSymlinks(source); err == nil {
			source = real
		}

		// Unix socket: mount parent directory (see buildItems comment).
		if fi, err := os.Stat(source); err == nil && fi.Mode()&os.ModeSocket != 0 {
			items = appendSocketMount(items, cfg, source, m)
			continue
		}

		createOpt := "create=dir"
		if fi, err := os.Stat(source); err == nil && !fi.IsDir() {
			createOpt = "create=file"
		}
		opts := "bind," + createOpt
		if m.ReadOnly {
			opts += ",ro"
		}
		dest := strings.TrimPrefix(m.Destination, "/")
		escapedSource := strings.ReplaceAll(source, " ", `\040`)
		escapedDest := strings.ReplaceAll(dest, " ", `\040`)
		entry := fmt.Sprintf("%s %s none %s 0 0", escapedSource, escapedDest, opts)
		items = append(items, configItem{"lxc.mount.entry", entry})
	}

	// Devices.
	for _, d := range cfg.Devices {
		dest := d.PathInContainer
		if dest == "" {
			dest = d.PathOnHost
		}
		destRel := strings.TrimPrefix(dest, "/")
		hostPath := d.PathOnHost
		if real, err := filepath.EvalSymlinks(hostPath); err == nil {
			hostPath = real
		}
		fi, err := os.Stat(hostPath)
		isDir := err == nil && fi.IsDir()
		if isDir {
			items = append(items, configItem{
				"lxc.mount.entry",
				fmt.Sprintf("%s %s none bind,create=dir 0 0", hostPath, destRel),
			})
			if entries, err := os.ReadDir(hostPath); err == nil {
				for _, entry := range entries {
					if entry.IsDir() {
						continue
					}
					if rule := deviceCgroupEntry(filepath.Join(hostPath, entry.Name())); rule != "" {
						items = append(items, configItem{"lxc.cgroup2.devices.allow", rule})
					}
				}
			}
		} else {
			if rule := deviceCgroupEntry(hostPath); rule != "" {
				items = append(items, configItem{"lxc.cgroup2.devices.allow", rule})
			}
			items = append(items, configItem{
				"lxc.mount.entry",
				fmt.Sprintf("%s %s none bind,create=file 0 0", hostPath, destRel),
			})
		}
	}

	// Device cgroup rules.
	for _, rule := range cfg.DeviceCgroupRules {
		items = append(items, configItem{"lxc.cgroup2.devices.allow", rule})
	}
	items = append(items, autoMountDeviceDirs(cfg.DeviceCgroupRules)...)

	// CPU (memory handled by Proxmox-native "memory:" line).
	if cfg.CPUShares > 0 {
		items = append(items, configItem{
			"lxc.cgroup2.cpu.weight",
			fmt.Sprintf("%d", cpuSharesToWeight(cfg.CPUShares)),
		})
	}
	if cfg.CPUQuota > 0 {
		items = append(items, configItem{
			"lxc.cgroup2.cpu.max",
			fmt.Sprintf("%d 100000", cfg.CPUQuota),
		})
	} else if cfg.NanoCPUs > 0 {
		quota := cfg.NanoCPUs * 100000 / 1_000_000_000
		if quota < 1000 {
			quota = 1000
		}
		items = append(items, configItem{
			"lxc.cgroup2.cpu.max",
			fmt.Sprintf("%d 100000", quota),
		})
	}
	if cfg.CpusetCpus != "" {
		items = append(items, configItem{"lxc.cgroup2.cpuset.cpus", cfg.CpusetCpus})
	}
	if cfg.CpusetMems != "" {
		items = append(items, configItem{"lxc.cgroup2.cpuset.mems", cfg.CpusetMems})
	}
	if cfg.PidsLimit > 0 {
		items = append(items, configItem{"lxc.cgroup2.pids.max", fmt.Sprintf("%d", cfg.PidsLimit)})
	}
	if cfg.BlkioWeight > 0 {
		items = append(items, configItem{"lxc.cgroup2.io.weight", fmt.Sprintf("default %d", cfg.BlkioWeight)})
	}

	// Capabilities / privileged: same rules as the legacy path.
	items = append(items, capabilityItems(cfg)...)
	items = append(items, securityOptItems(cfg)...)
	items = append(items, sysctlItems(cfg)...)
	items = append(items, tmpfsItems(cfg)...)
	items = append(items, ulimitItems(cfg)...)

	// Console log.
	if cfg.LogFile != "" {
		items = append(items, configItem{"lxc.console.logfile", cfg.LogFile})
	}

	return items
}

// LogFilePath returns the canonical console log file path for a container.
func LogFilePath(lxcPath, name string) string {
	return filepath.Join(lxcPath, "..", "docker-lxc-daemon", "logs", name+".log")
}
