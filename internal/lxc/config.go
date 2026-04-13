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
	MemoryBytes       int64        // 0 = unlimited
	CPUShares         int64        // 0 = unlimited (relative weight)
	CPUQuota          int64        // microseconds per 100ms period, 0 = unlimited
	WorkingDir        string       // container cwd; maps to lxc.init.cwd
	// LogFile is where the container console output is written.
	// Set automatically by the manager.
	LogFile string
	// ProxmoxCT requests that this container be created as a Proxmox CT
	// (visible in the Proxmox web UI). When false and PVE mode is active,
	// the container is created as an ephemeral raw-LXC container with a
	// ZFS-cloned rootfs — invisible to Proxmox but still on the PVE storage.
	ProxmoxCT bool
	// LAN requests a second NIC on the physical LAN bridge (e.g. vmbr0).
	// Only effective for Proxmox CTs — the LAN IP is derived from the VMID.
	LAN bool
	// LANBridge, LANIP, LANGateway are filled in by the manager (not the API
	// layer) when LAN is true and the daemon has --lan-bridge configured.
	LANBridge  string // e.g. "vmbr0"
	LANIP      string // e.g. "192.168.1.106/23"
	LANGateway string // e.g. "192.168.1.1"
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

// rewriteConfig reads the cloned LXC config file, strips problematic lines
// inherited from the download template (userns, apparmor, duplicate network),
// and appends the daemon-managed config items. This is more reliable than
// the go-lxc SetConfigItem API because lxc.include directives are processed
// at container start time and can override in-memory changes.
func rewriteConfig(path string, cfg ContainerConfig, ip, containerName string) error {
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

	// Resolve mount entry destinations against the container's rootfs so that
	// any symlinks (e.g. /var/run → /run) are followed. LXC rejects mount
	// entries whose destination paths traverse symlinks in the rootfs.
	rootfs := filepath.Join(filepath.Dir(path), "rootfs")
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

func buildItems(cfg ContainerConfig, ip string) []configItem {
	var items []configItem

	// Docker-compatible default mounts: /dev/shm (shared memory) is required
	// by most graphical apps (Wayland/wlroots), IPC, and many libraries.
	items = append(items, configItem{"lxc.mount.entry", "tmpfs dev/shm tmpfs rw,nosuid,nodev,create=dir 0 0"})

	// Network configuration.
	if cfg.LANBridge != "" {
		// Dual-NIC: internal bridge (gow0, no gateway) + LAN bridge (default route).
		// Used instead of host networking for containers that need LAN access
		// (e.g. Moonlight mDNS discovery) but run as Proxmox CTs where
		// lxc.namespace.clone is silently stripped.
		items = append(items, InternalNetworkConfig(ip)...)
		items = append(items, LANNetworkConfig(cfg.LANBridge, cfg.LANIP, cfg.LANGateway)...)
	} else if cfg.NetworkMode == "host" {
		// Share the host's network namespace by only cloning the other namespaces.
		// lxc.namespace.clone lists which namespaces to CREATE; omitting net
		// means the container inherits the host's network namespace.
		items = append(items, configItem{"lxc.namespace.clone", "ipc mnt pid uts"})
	} else {
		items = append(items, NetworkConfig(ip)...)
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

		// Detect whether source is a directory or a file/socket so we use
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
	}

	// Console log so we can serve it via the logs API
	if cfg.LogFile != "" {
		items = append(items, configItem{"lxc.console.logfile", cfg.LogFile})
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
func writePVEConfig(vmid int, hostname, rootfsSpec, rootfsPath string, cfg ContainerConfig, ip string) error {
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
func buildPVEItems(cfg ContainerConfig, ip string) []configItem {
	var items []configItem

	items = append(items, configItem{"lxc.apparmor.profile", "unconfined"})
	items = append(items, configItem{"lxc.mount.auto", ""})
	items = append(items, configItem{"lxc.mount.auto", "proc:mixed sys:mixed"})

	// /dev/shm
	items = append(items, configItem{"lxc.mount.entry", "tmpfs dev/shm tmpfs rw,nosuid,nodev,create=dir 0 0"})

	// Network configuration.
	if cfg.LANBridge != "" {
		// Dual-NIC: internal bridge (gow0) + physical LAN bridge.
		items = append(items, InternalNetworkConfig(ip)...)
		items = append(items, LANNetworkConfig(cfg.LANBridge, cfg.LANIP, cfg.LANGateway)...)
	} else if cfg.NetworkMode == "host" {
		items = append(items, configItem{"lxc.namespace.clone", "ipc mnt pid uts"})
	} else {
		items = append(items, NetworkConfig(ip)...)
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
	}

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
