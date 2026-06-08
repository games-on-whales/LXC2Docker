package lxc

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// nvidiaCDIPath is where we read/cache the NVIDIA CDI spec (JSON form, so we
// can parse it with the standard library and no YAML dependency).
const nvidiaCDIPath = "/etc/cdi/nvidia.json"

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

// loadNvidiaCDI reads the NVIDIA CDI spec, generating it once via nvidia-ctk if
// it isn't present yet (e.g. first GPU container after a fresh install).
func loadNvidiaCDI() (*cdiSpec, error) {
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
	// We bind the driver libraries and device nodes in; the SONAME symlinks and
	// ld cache are wired by the image's standard nvidia init (ldconfig), as in
	// the normal nvidia-container model. We deliberately do NOT emit an
	// lxc.hook.mount for the CDI create-symlinks/ldcache step: PVE silently
	// rejects custom lxc.hook.* directives, and that rejected line also breaks
	// pct's post-start PID query.
	return nvidiaItemsFromSpec(spec), nil
}

// nvidiaItemsFromSpec translates a CDI spec into LXC mount/device items: bind
// mounts for the driver libraries/binaries and the GPU device nodes (with cgroup
// allows). It applies the spec's global edits plus the "all" device's edits.
// Pure (no I/O beyond stat for dir detection) so it can be unit-tested.
func nvidiaItemsFromSpec(spec *cdiSpec) (items []configItem) {
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
	}

	return items
}
