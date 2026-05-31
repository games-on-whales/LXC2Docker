package lxc

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/games-on-whales/LXC2Docker/internal/oci"
	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// pveTemplateTarballPath returns the on-disk path of an image's rootfs tarball,
// kept under the daemon state dir (not Proxmox storage, so it never shows in the
// Proxmox UI).
func (m *Manager) pveTemplateTarballPath(ref string) string {
	return filepath.Join(m.store.RootDir(), "pve-templates", oci.SafeDirName(ref)+".tar.gz")
}

// detectPVEStorageType returns the backend type of a PVE storage (e.g.
// "zfspool", "lvmthin", "dir") by parsing `pvesm status`. Returns "" when the
// type can't be determined, which callers treat as non-ZFS (the safe,
// storage-agnostic path).
func detectPVEStorageType(storage string) string {
	out, err := exec.Command("pvesm", "status", "-storage", storage).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == storage {
			return fields[1]
		}
	}
	return ""
}

// pveStorageIsZFS reports whether the configured PVE storage is a ZFS pool,
// whose rootfs is a directory directly accessible at /<pool>/subvol-<vmid>-disk-0.
// Other backends (lvmthin, dir, …) are block devices that must be mounted via
// `pct mount` for offline preparation.
func (m *Manager) pveStorageIsZFS() bool { return m.pveStgType == "zfspool" }

// readPVERootfsSpec extracts the `rootfs:` volume spec from a CT's config — e.g.
// "storage:vm-123-disk-0,size=4G". This is the volume `pct create` chose for the
// backing storage (subvol-<vmid> on zfs/dir, vm-<vmid> on lvmthin), preserved
// verbatim when the daemon rewrites the config.
func readPVERootfsSpec(vmid int) (string, error) {
	data, err := os.ReadFile(pveConfigPath(vmid))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "rootfs:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "rootfs:")), nil
		}
	}
	return "", fmt.Errorf("no rootfs entry in CT %d config", vmid)
}

// mountPVERootfs makes a CT's rootfs accessible for offline preparation and
// returns its path plus an idempotent unmount func. On ZFS the subvol is already
// mounted at a stable path; on block-device backends (lvmthin, …) it is mounted
// via `pct mount` and must be unmounted before the container can start.
func (m *Manager) mountPVERootfs(vmid int) (string, func(), error) {
	if m.pveStorageIsZFS() {
		return m.pveRootfsPath(vmid), func() {}, nil
	}
	if out, err := exec.Command("pct", "mount", fmt.Sprintf("%d", vmid)).CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("pct mount %d: %s: %w", vmid, strings.TrimSpace(string(out)), err)
	}
	var once bool
	unmount := func() {
		if once {
			return
		}
		once = true
		exec.Command("pct", "unmount", fmt.Sprintf("%d", vmid)).Run()
	}
	return fmt.Sprintf("/var/lib/lxc/%d/rootfs", vmid), unmount, nil
}

// dirSizeGB returns the apparent on-disk size of a tarball's contents in whole
// gigabytes. We size the CT rootfs from the tarball file size as a cheap proxy,
// padded generously, with a 4G floor.
func tarballRootfsGB(tarball string) int {
	st, err := os.Stat(tarball)
	if err != nil {
		return 4
	}
	// Compressed tar is ~2-3x smaller than the rootfs; pad to be safe.
	gb := int(st.Size()/(1<<30))*3 + 4
	if gb < 4 {
		gb = 4
	}
	return gb
}

// createPVEFromTarball creates a Proxmox CT directly from the image's rootfs
// tarball via `pct create` — storage-agnostic (LVM/ZFS/dir) and needing no
// Proxmox template CT, so templates never appear in the Proxmox UI. The
// container itself is a normal CT, visible in the UI as expected.
func (m *Manager) createPVEFromTarball(id string, imgRec *store.ImageRecord, cfg ContainerConfig) error {
	vmid, err := allocateVMID()
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}

	if cfg.LAN && m.lan.Bridge != "" {
		cfg.LANBridge = m.lan.Bridge
		cfg.LANIP = fmt.Sprintf("%s.%d/%d", m.lan.Prefix, vmid, m.lan.Subnet)
		cfg.LANGateway = m.lan.Gateway
	}

	hostname := id[:12]
	if storeRec := m.store.GetContainer(id); storeRec != nil {
		hostname = storeRec.Name
	}
	hostname = sanitizeHostname(hostname)

	sizeGB := tarballRootfsGB(imgRec.TemplateTarball)
	log.Printf("CreateContainer[PVE]: pct create %d from tarball for %s (rootfs %dG)", vmid, id[:12], sizeGB)
	if out, err := exec.Command("pct", "create", fmt.Sprintf("%d", vmid), imgRec.TemplateTarball,
		"--storage", m.pveStorage,
		"--ostype", "unmanaged",
		"--arch", "amd64",
		"--hostname", hostname,
		"--unprivileged", "0",
		"--rootfs", fmt.Sprintf("%s:%d", m.pveStorage, sizeGB),
	).CombinedOutput(); err != nil {
		return fmt.Errorf("manager: pct create %d from tarball: %s: %w", vmid, out, err)
	}
	cleanup := func() { exec.Command("pct", "destroy", fmt.Sprintf("%d", vmid), "--force").Run() }

	var ip string
	if cfg.NetworkMode != "host" {
		ip, err = m.store.AllocateIP()
		if err != nil {
			cleanup()
			return fmt.Errorf("manager: allocate IP: %w", err)
		}
	}

	cfg.LogFile = LogFilePath(m.lxcPath, id)
	if err := os.MkdirAll(filepath.Dir(cfg.LogFile), 0o755); err != nil {
		cleanup()
		return fmt.Errorf("manager: mkdir log dir: %w", err)
	}

	// Preserve the rootfs volume pct create chose (storage-specific naming).
	rootfsSpec, err := readPVERootfsSpec(vmid)
	if err != nil {
		cleanup()
		return fmt.Errorf("manager: read rootfs spec: %w", err)
	}
	rootfsPath, unmount, err := m.mountPVERootfs(vmid)
	if err != nil {
		cleanup()
		return fmt.Errorf("manager: mount rootfs: %w", err)
	}

	if err := writePVEConfig(vmid, hostname, rootfsSpec, rootfsPath, &cfg, ip); err != nil {
		unmount()
		cleanup()
		return fmt.Errorf("manager: write PVE config: %w", err)
	}
	m.prepareRootfs(rootfsPath, cfg)
	unmount() // a held mount blocks pct start on block-device backends

	if storeRec := m.store.GetContainer(id); storeRec != nil {
		storeRec.IPAddress = ip
		storeRec.VMID = vmid
		return m.store.AddContainer(storeRec)
	}
	return nil
}
