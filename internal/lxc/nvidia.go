package lxc

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// NVIDIA GPU support via CDI (Container Device Interface).
//
// Instead of relying on a hand-populated "/usr/nvidia" driver volume, the daemon
// reads the NVIDIA CDI spec produced by the nvidia-container-toolkit
// (`nvidia-ctk cdi generate`) and translates it into LXC config: bind mounts for
// every host driver library/binary, the GPU device nodes (bind + cgroup allow),
// and an lxc.hook.mount that recreates the driver symlinks and refreshes the ld
// cache inside the container — i.e. the same result the OCI nvidia runtime hooks
// produce, but expressed as LXC primitives.

const (
	// nvidiaCDIPath is where we read/cache the NVIDIA CDI spec (JSON form, so we
	// can parse it with the standard library and no YAML dependency).
	nvidiaCDIPath = "/etc/cdi/nvidia.json"
	// nvidiaMountHookPath is the generated lxc.hook.mount script (symlinks + ldcache).
	nvidiaMountHookPath = "/etc/cdi/lxc-nvidia-mount-hook.sh"
)

// cdiSpec is the subset of a CDI spec we consume (NVIDIA's nvidia.com/gpu kind).
type cdiSpec struct {
	ContainerEdits cdiEdits    `json:"containerEdits"`
	Devices        []cdiDevice `json:"devices"`
}

type cdiDevice struct {
	Name           string   `json:"name"`
	ContainerEdits cdiEdits `json:"containerEdits"`
}

type cdiEdits struct {
	DeviceNodes []cdiDeviceNode `json:"deviceNodes"`
	Mounts      []cdiMount      `json:"mounts"`
	Hooks       []cdiHook       `json:"hooks"`
}

type cdiDeviceNode struct {
	Path  string `json:"path"`
	Major int    `json:"major"`
	Minor int    `json:"minor"`
}

type cdiMount struct {
	HostPath      string   `json:"hostPath"`
	ContainerPath string   `json:"containerPath"`
	Options       []string `json:"options"`
}

type cdiHook struct {
	HookName string   `json:"hookName"`
	Path     string   `json:"path"`
	Args     []string `json:"args"`
}

// The CDI spec is static between driver changes but every GPU-container launch
// re-reads and re-parses it. Cache the parsed spec keyed by the file's mtime+size
// so re-opens skip the read/parse; a driver update (nvidia-ctk regenerates the
// file, changing mtime) invalidates it automatically.
var (
	cdiCacheMu   sync.Mutex
	cdiCacheKey  string // "<modtime-unixnano>:<size>"
	cdiCacheSpec *cdiSpec
)

// loadNvidiaCDI reads the NVIDIA CDI spec, generating it once via nvidia-ctk if
// it isn't present yet (e.g. first GPU container after a fresh install). The
// parsed result is cached by file mtime+size across launches.
func loadNvidiaCDI() (*cdiSpec, error) {
	if fi, err := os.Stat(nvidiaCDIPath); err == nil {
		key := fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size())
		cdiCacheMu.Lock()
		if cdiCacheSpec != nil && cdiCacheKey == key {
			spec := cdiCacheSpec
			cdiCacheMu.Unlock()
			return spec, nil
		}
		cdiCacheMu.Unlock()
	}

	data, err := os.ReadFile(nvidiaCDIPath)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(nvidiaCDIPath), 0o755); err != nil {
			return nil, err
		}
		if out, gerr := exec.Command("nvidia-ctk", "cdi", "generate",
			"--format=json", "--output="+nvidiaCDIPath).CombinedOutput(); gerr != nil {
			return nil, fmt.Errorf("nvidia-ctk cdi generate: %s: %w", strings.TrimSpace(string(out)), gerr)
		}
		data, err = os.ReadFile(nvidiaCDIPath)
	}
	if err != nil {
		return nil, err
	}
	var spec cdiSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse nvidia CDI spec %s: %w", nvidiaCDIPath, err)
	}

	// Cache under the just-read file's current mtime+size (re-stat: the generate
	// branch above may have just created it).
	if fi, err := os.Stat(nvidiaCDIPath); err == nil {
		cdiCacheMu.Lock()
		cdiCacheKey = fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size())
		cdiCacheSpec = &spec
		cdiCacheMu.Unlock()
	}
	return &spec, nil
}

// nvidiaGPUConfigItems returns the lxc.* config items that inject the host
// NVIDIA driver into a GPU container. It loads the CDI spec, translates it, and
// writes the symlink/ldcache mount hook.
func nvidiaGPUConfigItems() ([]configItem, error) {
	spec, err := loadNvidiaCDI()
	if err != nil {
		return nil, err
	}
	items, links := nvidiaItemsFromSpec(spec)
	if err := writeNvidiaMountHook(links); err != nil {
		return nil, err
	}
	items = append(items, configItem{"lxc.hook.mount", nvidiaMountHookPath})
	return items, nil
}

// nvidiaItemsFromSpec translates a CDI spec into LXC mount/device items and the
// list of create-symlinks "target::link" pairs. It applies the spec's global
// edits plus the "all" device's edits (driver libs + every GPU's device nodes).
// Pure (no I/O beyond stat for dir detection) so it can be unit-tested.
func nvidiaItemsFromSpec(spec *cdiSpec) (items []configItem, links []string) {
	edits := []cdiEdits{spec.ContainerEdits}
	for _, d := range spec.Devices {
		if d.Name == "all" {
			edits = append(edits, d.ContainerEdits)
		}
	}

	seenMount := map[string]bool{}
	seenDev := map[string]bool{}

	for _, e := range edits {
		// Driver libraries / binaries / config files.
		for _, mnt := range e.Mounts {
			if mnt.HostPath == "" || mnt.ContainerPath == "" || seenMount[mnt.ContainerPath] {
				continue
			}
			seenMount[mnt.ContainerPath] = true
			createOpt := "create=file"
			if fi, err := os.Stat(mnt.HostPath); err == nil && fi.IsDir() {
				createOpt = "create=dir"
			}
			src := strings.ReplaceAll(mnt.HostPath, " ", `\040`)
			dest := strings.ReplaceAll(strings.TrimPrefix(mnt.ContainerPath, "/"), " ", `\040`)
			items = append(items, configItem{
				"lxc.mount.entry",
				fmt.Sprintf("%s %s none bind,%s,ro 0 0", src, dest, createOpt),
			})
		}
		// GPU device nodes: allow in the cgroup and bind the node in.
		for _, dn := range e.DeviceNodes {
			if dn.Path == "" || seenDev[dn.Path] {
				continue
			}
			seenDev[dn.Path] = true
			if dn.Major > 0 {
				items = append(items, configItem{
					"lxc.cgroup2.devices.allow",
					fmt.Sprintf("c %d:%d rwm", dn.Major, dn.Minor),
				})
			}
			dest := strings.ReplaceAll(strings.TrimPrefix(dn.Path, "/"), " ", `\040`)
			items = append(items, configItem{
				"lxc.mount.entry",
				fmt.Sprintf("%s %s none bind,create=file 0 0", dn.Path, dest),
			})
		}
		// Collect create-symlinks --link pairs (target::link) for the mount hook.
		for _, h := range e.Hooks {
			links = append(links, parseCDISymlinks(h.Args)...)
		}
	}

	return items, links
}

// parseCDISymlinks extracts the "target::link" values that follow each --link
// flag in a create-symlinks hook's argument list.
func parseCDISymlinks(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--link" && i+1 < len(args) {
			out = append(out, args[i+1])
			i++
		}
	}
	return out
}

// writeNvidiaMountHook (idempotently) writes the lxc.hook.mount script that
// recreates the driver symlinks and refreshes the ld cache inside the
// container's mounted rootfs ($LXC_ROOTFS_MOUNT). This replicates the CDI
// create-symlinks + update-ldcache hooks using plain shell, so it needs nothing
// from the container image beyond ldconfig. It must never fail the mount stage,
// hence the `|| true` guards and the final `exit 0`.
func writeNvidiaMountHook(links []string) error {
	return writeNvidiaMountHookTo(nvidiaMountHookPath, links)
}

func writeNvidiaMountHookTo(path string, links []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Generated by docker-lxc-daemon: inject NVIDIA driver symlinks + ldcache.\n")
	b.WriteString("# Runs as an lxc.hook.mount; $LXC_ROOTFS_MOUNT is the container rootfs.\n")
	b.WriteString("R=\"$LXC_ROOTFS_MOUNT\"\n")
	b.WriteString("[ -n \"$R\" ] || exit 0\n")
	seen := map[string]bool{}
	for _, l := range links {
		parts := strings.SplitN(l, "::", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		target := parts[0]
		link := "/" + strings.TrimPrefix(parts[1], "/")
		if seen[link] {
			continue
		}
		seen[link] = true
		// nvidia driver paths contain no spaces/shell metacharacters.
		b.WriteString(fmt.Sprintf("mkdir -p \"$R%s\" 2>/dev/null || true\n", filepath.Dir(link)))
		b.WriteString(fmt.Sprintf("ln -sf \"%s\" \"$R%s\" 2>/dev/null || true\n", target, link))
	}
	// Refresh the ld cache so freshly bind-mounted libs resolve by SONAME.
	b.WriteString("chroot \"$R\" /sbin/ldconfig 2>/dev/null || ldconfig -r \"$R\" 2>/dev/null || true\n")
	b.WriteString("exit 0\n")
	return os.WriteFile(path, []byte(b.String()), 0o755)
}
