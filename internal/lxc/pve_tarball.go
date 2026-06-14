package lxc

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/oci"
	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// orphanCTGrace is how long a tagged-but-unreferenced CT must survive before
// the reaper destroys it. createPVEFromTarball writes the management tag (via
// writePVEConfig) before it sets the store record's VMID, so a CT mid-creation
// briefly looks like an orphan; the grace window — far longer than any create
// takes — ensures we never reap one that's still being built.
const orphanCTGrace = 5 * time.Minute

// managedCTVMIDs returns the VMIDs of every Proxmox CT tagged as daemon-managed.
// It scans /etc/pve/lxc/*.conf for ManagedTag rather than shelling out to pct,
// so it works regardless of CT run state. Crucially, only CTs that carry the
// tag are returned — anything the daemon didn't create is never surfaced, which
// keeps every caller (notably the reaper) scoped strictly to our own CTs.
func managedCTVMIDs() []int {
	entries, err := filepath.Glob("/etc/pve/lxc/*.conf")
	if err != nil {
		return nil
	}
	var vmids []int
	for _, path := range entries {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if !confHasManagedTag(data) {
			continue
		}
		base := strings.TrimSuffix(filepath.Base(path), ".conf")
		vmid, err := strconv.Atoi(base)
		if err != nil {
			continue
		}
		vmids = append(vmids, vmid)
	}
	return vmids
}

// confHasManagedTag reports whether a CT config's "tags:" line includes the
// daemon's ManagedTag. Proxmox stores multiple tags semicolon-separated.
func confHasManagedTag(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "tags:") {
			continue
		}
		tags := strings.TrimSpace(strings.TrimPrefix(line, "tags:"))
		for _, t := range strings.Split(tags, ";") {
			if strings.TrimSpace(t) == ManagedTag {
				return true
			}
		}
	}
	return false
}

// reapOrphanCTs destroys Proxmox CTs that carry the daemon's management tag but
// have no backing store record. These are leaks from abnormal exits — a daemon
// crash mid-create, or a RemoveContainer that dropped the store entry before
// pct destroy succeeded — and otherwise linger forever in the Proxmox UI.
//
// Safety is layered: (1) only tagged CTs are ever considered, so nothing the
// daemon didn't create can be touched; (2) a grace period skips CTs whose
// config was just written (a container mid-creation); (3) the store is
// re-checked immediately before destroy to close any remaining race.
func (m *Manager) reapOrphanCTs() {
	if !m.UsePVE() {
		return
	}

	referenced := map[int]bool{}
	for _, rec := range m.store.ListContainers() {
		if rec.VMID > 0 {
			referenced[rec.VMID] = true
		}
	}
	for _, img := range m.store.ListImages() {
		if img.TemplateVMID > 0 {
			referenced[img.TemplateVMID] = true
		}
	}

	for _, vmid := range managedCTVMIDs() {
		if referenced[vmid] {
			continue
		}
		fi, err := os.Stat(pveConfigPath(vmid))
		if err != nil || time.Since(fi.ModTime()) < orphanCTGrace {
			continue
		}
		// Final guard: re-scan the store right before destroying, in case a
		// create completed between the snapshot above and now.
		if m.vmidReferenced(vmid) {
			continue
		}
		log.Printf("reapOrphanCTs: destroying leaked managed CT %d (no backing container)", vmid)
		if out, err := exec.Command("pct", "destroy", fmt.Sprintf("%d", vmid), "--force").CombinedOutput(); err != nil {
			log.Printf("reapOrphanCTs: pct destroy %d: %s: %v", vmid, out, err)
		}
	}
}

// vmidReferenced reports whether any current store record points at vmid.
func (m *Manager) vmidReferenced(vmid int) bool {
	for _, rec := range m.store.ListContainers() {
		if rec.VMID == vmid {
			return true
		}
	}
	for _, img := range m.store.ListImages() {
		if img.TemplateVMID == vmid {
			return true
		}
	}
	return false
}

// reapOrphanTarballs deletes image rootfs tarballs under pve-templates/ that no
// image record references. This is the tarball analogue of reapOrphanCTs: if an
// image record is lost (a crash, or a scheme migration) RemoveImage never runs
// for it and the .tar.gz would otherwise linger forever, consuming disk.
//
// pullOCI writes the tarball before it AddImage's the record, so a pull in
// flight briefly has an unreferenced tarball; the same grace window used for
// CTs (orphanCTGrace) skips recently-written tarballs, and the store is
// re-checked immediately before each delete.
func (m *Manager) reapOrphanTarballs() {
	if !m.UsePVE() {
		return
	}
	dir := filepath.Join(m.CacheDir(), "pve-templates")
	entries, err := filepath.Glob(filepath.Join(dir, "*.tar.gz"))
	if err != nil {
		return
	}

	referenced := map[string]bool{}
	for _, img := range m.store.ListImages() {
		if img.TemplateTarball != "" {
			referenced[img.TemplateTarball] = true
		}
	}

	for _, path := range entries {
		if referenced[path] {
			continue
		}
		fi, err := os.Stat(path)
		if err != nil || time.Since(fi.ModTime()) < orphanCTGrace {
			continue
		}
		// Final guard: re-scan in case an AddImage landed since the snapshot.
		if m.tarballReferenced(path) {
			continue
		}
		log.Printf("reapOrphanTarballs: deleting leaked image tarball %s (no backing image record)", path)
		if err := os.Remove(path); err != nil {
			log.Printf("reapOrphanTarballs: remove %s: %v", path, err)
		}
	}
}

// tarballReferenced reports whether any current image record points at path.
func (m *Manager) tarballReferenced(path string) bool {
	for _, img := range m.store.ListImages() {
		if img.TemplateTarball == path {
			return true
		}
	}
	return false
}

// pveTemplateTarballPath returns the on-disk path of an image's rootfs tarball,
// kept under the daemon state dir (not Proxmox storage, so it never shows in the
// Proxmox UI).
func (m *Manager) pveTemplateTarballPath(ref string) string {
	return filepath.Join(m.CacheDir(), "pve-templates", oci.SafeDirName(ref)+".tar.gz")
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

// pveStorageSupportsLinkedClone reports whether the configured PVE storage can
// back instant copy-on-write linked CT clones via `pct clone` (no --full). Only
// lvmthin is enabled: ZFS uses its own raw-clone fast path, while dir/lvm/nfs
// have no CT snapshot primitive and `pct clone` falls back to a full copy there.
func (m *Manager) pveStorageSupportsLinkedClone() bool { return m.pveStgType == "lvmthin" }

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

// BuildTarballFromContainer captures a stopped build container's rootfs as an
// image tarball under pve-templates/, then destroys the build CT. The classic
// builder's finalize step uses it on PVE: a build container is a CT (an LVM/ZFS
// volume), not a plain directory, so its rootfs is reached via pct mount and
// tarred — yielding the same tarball-backed image scheme as pullOCI (hidden
// from the Proxmox UI, reaped by reapOrphanTarballs, run via
// createPVEFromTarball). Returns the tarball path on success.
func (m *Manager) BuildTarballFromContainer(tmpID, ref string) (string, error) {
	rec := m.store.GetContainer(tmpID)
	if rec == nil || rec.VMID == 0 {
		return "", fmt.Errorf("manager: build container %s has no PVE CT to capture", tmpID)
	}

	tarball := m.pveTemplateTarballPath(ref)
	if err := os.MkdirAll(filepath.Dir(tarball), 0o755); err != nil {
		return "", fmt.Errorf("manager: mkdir tarball dir: %w", err)
	}
	os.Remove(tarball) // clear any stale tarball from a prior build of this ref

	rootfsPath, unmount, err := m.mountPVERootfs(rec.VMID)
	if err != nil {
		return "", fmt.Errorf("manager: mount build rootfs: %w", err)
	}
	tarErr := func() error {
		if out, err := exec.Command("tar", "czf", tarball, "-C", rootfsPath, ".").CombinedOutput(); err != nil {
			return fmt.Errorf("tar build rootfs: %s: %w", strings.TrimSpace(string(out)), err)
		}
		return nil
	}()
	unmount() // must release the mount before pct destroy on block-device backends
	if tarErr != nil {
		os.Remove(tarball)
		return "", fmt.Errorf("manager: %w", tarErr)
	}

	// The rootfs is captured; destroy the build CT (pct destroy via RemoveContainer).
	if err := m.RemoveContainer(tmpID); err != nil {
		log.Printf("BuildTarballFromContainer: remove build CT %s: %v (continuing)", tmpID, err)
	}
	log.Printf("BuildTarballFromContainer: stored built image tarball %s for %s", tarball, ref)
	return tarball, nil
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

// resolveRootfsGB picks the rootfs size for a `pct create` from a tarball,
// honoring an explicit cfg.DiskSizeGB (from --storage-opt size / dld.disksize)
// when set. The explicit size is floored at the image-derived minimum so the
// unpacked rootfs always fits — a too-small request is bumped (and logged)
// rather than failing the create.
func resolveRootfsGB(cfg ContainerConfig, tarball string) int {
	min := tarballRootfsGB(tarball)
	if cfg.DiskSizeGB <= 0 {
		return min
	}
	if cfg.DiskSizeGB < min {
		log.Printf("resolveRootfsGB: requested %dG < image minimum %dG; using %dG", cfg.DiskSizeGB, min, min)
		return min
	}
	return cfg.DiskSizeGB
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

	// Record the VMID in the store record immediately. The `pct create` below
	// takes several seconds before the CT is started; until the VMID is
	// recorded, the periodic gc() sees a VMID==0 record in the "exited" state
	// and reaps it mid-create, so the subsequent start fails with "No such
	// container". Recording it up front makes gc() treat the record as a
	// Proxmox CT (rec.VMID > 0) and skip it. A genuinely failed create is
	// cleaned up by the caller's error path and the reapOrphanCTs grace window.
	if storeRec := m.store.GetContainer(id); storeRec != nil {
		storeRec.VMID = vmid
		if err := m.store.AddContainer(storeRec); err != nil {
			return fmt.Errorf("manager: persist vmid: %w", err)
		}
	}

	// Fill in LAN config (and convert --network=host to a LAN NIC) before the
	// IP allocation below keys off cfg.NetworkMode.
	applyLANNetworking(&cfg, m.lan, vmid, m.lanMACForContainer(id))

	hostname := id[:12]
	if storeRec := m.store.GetContainer(id); storeRec != nil {
		hostname = storeRec.Name
	}
	hostname = sanitizeHostname(hostname)

	sizeGB := resolveRootfsGB(cfg, imgRec.TemplateTarball)
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
	return m.finalizePVECT(id, vmid, hostname, cfg)
}

// finalizePVECT configures an already-created Proxmox CT (vmid) — networking, IP,
// config, rootfs prep — and persists its store record. Shared by the pct-create
// and linked-clone tarball paths, which differ only in how the CT's rootfs is
// provisioned. On any error it destroys the CT.
func (m *Manager) finalizePVECT(id string, vmid int, hostname string, cfg ContainerConfig) error {
	cleanup := func() { exec.Command("pct", "destroy", fmt.Sprintf("%d", vmid), "--force").Run() }

	var ip string
	if cfg.NetworkMode != "host" {
		var err error
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

	// Preserve the rootfs volume pct chose (storage-specific naming).
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

	// Docker-compat self-identification: bind /etc/hostname from a path that
	// embeds the container ID, so an app can find its own container by scanning
	// /proc/self/mountinfo (e.g. Wolf's get_current_container, which replicates
	// its own mounts into the app containers it launches). Done here in the
	// shared finalizer so EVERY PVE path gets it — previously only
	// createPVEContainer wired this up, so the common lvmthin/tarball
	// linked-clone path produced CTs that couldn't self-identify ("No valid
	// container ID found in mountinfo").
	if src, err := m.writeContainerIDHostname(id, hostname); err != nil {
		log.Printf("finalizePVECT: container ID hostname for %s: %v", shortID(id), err)
	} else {
		cfg.IDHostnameSource = src
	}

	if err := writePVEConfig(vmid, hostname, rootfsSpec, rootfsPath, &cfg, ip, m.DefaultMemoryBytes); err != nil {
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

// reconfigureReusedPVECT adopts an already-provisioned Proxmox CT (cfg.ReuseVMID,
// the warm rootfs from a previous session of the same app) for a new session: it
// rewrites the CT's config and re-preps the rootfs for the new launch WITHOUT
// cloning — the single biggest re-open latency win, since the expensive clone is
// skipped and the in-rootfs warm state (app config, shader caches) is preserved.
// The container keeps its existing IP. Returns an error (→ caller falls back to a
// fresh clone) if the CT has vanished since it was recorded.
func (m *Manager) reconfigureReusedPVECT(id string, cfg ContainerConfig) error {
	vmid := cfg.ReuseVMID
	if _, err := os.Stat(pveConfigPath(vmid)); err != nil {
		return fmt.Errorf("reused CT %d config missing: %w", vmid, err)
	}
	if st, _ := m.State(id); st == "running" {
		return fmt.Errorf("reused CT %d is running", vmid)
	}

	rec := m.store.GetContainer(id)
	hostname := id[:12]
	ip := ""
	if rec != nil {
		if rec.Name != "" {
			hostname = rec.Name
		}
		ip = rec.IPAddress // keep the address the CT already holds
	}
	hostname = sanitizeHostname(hostname)

	// Fill LAN config (may convert --network=host to a LAN NIC) before writing.
	applyLANNetworking(&cfg, m.lan, vmid, m.lanMACForContainer(id))

	cfg.LogFile = LogFilePath(m.lxcPath, id)
	if err := os.MkdirAll(filepath.Dir(cfg.LogFile), 0o755); err != nil {
		return fmt.Errorf("mkdir log dir: %w", err)
	}

	rootfsSpec, err := readPVERootfsSpec(vmid)
	if err != nil {
		return fmt.Errorf("read rootfs spec: %w", err)
	}
	rootfsPath, unmount, err := m.mountPVERootfs(vmid)
	if err != nil {
		return fmt.Errorf("mount rootfs: %w", err)
	}

	if src, err := m.writeContainerIDHostname(id, hostname); err != nil {
		log.Printf("reconfigureReusedPVECT: container ID hostname for %s: %v", shortID(id), err)
	} else {
		cfg.IDHostnameSource = src
	}

	if err := writePVEConfig(vmid, hostname, rootfsSpec, rootfsPath, &cfg, ip, m.DefaultMemoryBytes); err != nil {
		unmount()
		return fmt.Errorf("write PVE config: %w", err)
	}
	m.prepareRootfs(rootfsPath, cfg)
	unmount() // a held mount blocks pct start on block-device backends

	log.Printf("CreateContainer[reuse]: adopted warm CT %d for %s (no clone)", vmid, shortID(id))
	if rec != nil {
		rec.VMID = vmid
		rec.IPAddress = ip
		return m.store.AddContainer(rec)
	}
	return nil
}

// sanitizeDatasetToken reduces an arbitrary string to characters valid in a ZFS
// dataset component ([A-Za-z0-9_.-]), so an image ID like "ubuntu_22.04" or an
// OCI ref with slashes/colons yields a stable, legal dataset name.
func sanitizeDatasetToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "img"
	}
	return out
}

// imageTemplateDataset is the ZFS dataset (under the PVE pool) that caches an
// image's unpacked rootfs for copy-on-write cloning. One dataset per image; its
// @base snapshot is the clone origin for every container of that image.
func (m *Manager) imageTemplateDataset(imgRec *store.ImageRecord) string {
	return fmt.Sprintf("%s/dld-tmpl-%s", m.pveStorage, sanitizeDatasetToken(imgRec.ID))
}

// ensureImageTemplateDataset materializes an image's rootfs tarball into its
// template dataset exactly once and returns the @base snapshot to clone from.
// Subsequent calls (and launches after a daemon restart) detect the existing
// snapshot and return immediately. Serialized by tmplMu so concurrent launches
// of the same image can't race the create/unpack/snapshot sequence.
func (m *Manager) ensureImageTemplateDataset(imgRec *store.ImageRecord) (string, error) {
	m.tmplMu.Lock()
	defer m.tmplMu.Unlock()

	ds := m.imageTemplateDataset(imgRec)
	snap := ds + "@base"
	if exec.Command("zfs", "list", "-t", "snapshot", "-H", "-o", "name", snap).Run() == nil {
		return snap, nil // already materialized
	}

	mountpoint := "/" + ds
	if out, err := exec.Command("zfs", "create", "-o", "mountpoint="+mountpoint, ds).CombinedOutput(); err != nil {
		return "", fmt.Errorf("zfs create %s: %s: %w", ds, strings.TrimSpace(string(out)), err)
	}
	if out, err := exec.Command("tar", "xzf", imgRec.TemplateTarball, "-C", mountpoint).CombinedOutput(); err != nil {
		exec.Command("zfs", "destroy", "-r", ds).Run()
		return "", fmt.Errorf("unpack %s → %s: %s: %w", imgRec.TemplateTarball, ds, strings.TrimSpace(string(out)), err)
	}
	if out, err := exec.Command("zfs", "snapshot", snap).CombinedOutput(); err != nil {
		exec.Command("zfs", "destroy", "-r", ds).Run()
		return "", fmt.Errorf("zfs snapshot %s: %s: %w", snap, strings.TrimSpace(string(out)), err)
	}

	// Record the dataset on the image so RemoveImage can clean it up.
	if rec := m.store.GetImage(imgRec.Ref); rec != nil {
		rec.TemplateDataset = ds
		if err := m.store.AddImage(rec); err != nil {
			log.Printf("ensureImageTemplateDataset: persist dataset on %s: %v (continuing)", imgRec.Ref, err)
		}
	}
	log.Printf("ensureImageTemplateDataset: materialized %s for %s", snap, imgRec.Ref)
	return snap, nil
}

// createZFSCloneFromTarball provisions an ephemeral container by copy-on-write
// cloning the image's template dataset — near-instant versus re-extracting the
// tarball with `pct create`. On any preparation failure it falls back to the
// storage-agnostic pct-create path so a launch never hard-fails on the fast
// path. An explicit DiskSizeGB becomes a `refquota` cap on the clone; without
// one the container can grow into the whole pool (thin), which is what gives a
// Steam container real room.
func (m *Manager) createZFSCloneFromTarball(id string, imgRec *store.ImageRecord, cfg ContainerConfig) error {
	snap, err := m.ensureImageTemplateDataset(imgRec)
	if err != nil {
		log.Printf("createZFSCloneFromTarball: template prep failed (%v); falling back to pct create", err)
		return m.createPVEFromTarball(id, imgRec, cfg)
	}

	cloneDataset := fmt.Sprintf("%s/lxc-%s", m.pveStorage, id)
	cloneMountpoint := fmt.Sprintf("/%s/lxc-%s", m.pveStorage, id)
	log.Printf("CreateContainer[zfs-clone]: %s → %s for %s", snap, cloneDataset, id[:12])
	if out, err := exec.Command("zfs", "clone", "-o", "mountpoint="+cloneMountpoint, snap, cloneDataset).CombinedOutput(); err != nil {
		log.Printf("createZFSCloneFromTarball: zfs clone failed (%s: %v); falling back to pct create", strings.TrimSpace(string(out)), err)
		return m.createPVEFromTarball(id, imgRec, cfg)
	}

	if cfg.DiskSizeGB > 0 {
		if out, err := exec.Command("zfs", "set", fmt.Sprintf("refquota=%dG", cfg.DiskSizeGB), cloneDataset).CombinedOutput(); err != nil {
			log.Printf("createZFSCloneFromTarball: set refquota on %s: %s: %v (continuing)", cloneDataset, strings.TrimSpace(string(out)), err)
		}
	}

	return m.finalizeRawZFSClone(id, cloneDataset, cloneMountpoint, cfg)
}

// imageTemplateCTHostname is the (cosmetic) hostname for an image's template CT,
// so it's identifiable in the Proxmox UI.
func imageTemplateCTHostname(imgRec *store.ImageRecord) string {
	return sanitizeHostname("dld-tmpl-" + sanitizeDatasetToken(imgRec.ID))
}

// ensureImageTemplateCT materializes an image's rootfs tarball into a Proxmox
// template CT exactly once and returns its VMID. The template's base volume is
// the copy-on-write origin that every container of that image is linked-cloned
// from. Subsequent calls (and launches after a daemon restart) detect the
// existing template and return immediately. Serialized by tmplMu so concurrent
// launches of the same image can't race the create/template sequence.
//
// Unlike the ZFS scheme (a hidden dataset), a CT template is visible in the
// Proxmox UI — the cost of staying on the supported `pct clone` path for
// block-device storages that expose no raw clone primitive the daemon can drive.
func (m *Manager) ensureImageTemplateCT(imgRec *store.ImageRecord) (int, error) {
	m.tmplMu.Lock()
	defer m.tmplMu.Unlock()

	// Already materialized? (record carries the VMID and its config still exists)
	if rec := m.store.GetImage(imgRec.Ref); rec != nil && rec.TemplateVMID > 0 {
		if _, err := os.Stat(pveConfigPath(rec.TemplateVMID)); err == nil {
			return rec.TemplateVMID, nil
		}
	}

	vmid, err := allocateVMID()
	if err != nil {
		return 0, fmt.Errorf("manager: %w", err)
	}

	sizeGB := tarballRootfsGB(imgRec.TemplateTarball)
	log.Printf("ensureImageTemplateCT: pct create template %d from tarball for %s (rootfs %dG)", vmid, imgRec.Ref, sizeGB)
	if out, err := exec.Command("pct", "create", fmt.Sprintf("%d", vmid), imgRec.TemplateTarball,
		"--storage", m.pveStorage,
		"--ostype", "unmanaged",
		"--arch", "amd64",
		"--hostname", imageTemplateCTHostname(imgRec),
		"--unprivileged", "0",
		"--rootfs", fmt.Sprintf("%s:%d", m.pveStorage, sizeGB),
	).CombinedOutput(); err != nil {
		return 0, fmt.Errorf("manager: pct create template %d: %s: %w", vmid, strings.TrimSpace(string(out)), err)
	}
	destroy := func() { exec.Command("pct", "destroy", fmt.Sprintf("%d", vmid), "--force").Run() }

	// Tag the template so reapOrphanCTs recognizes it as ours and can clean it
	// up if the image record is lost before TemplateVMID is persisted below.
	if out, err := exec.Command("pct", "set", fmt.Sprintf("%d", vmid), "--tags", ManagedTag).CombinedOutput(); err != nil {
		log.Printf("ensureImageTemplateCT: tag template %d: %s: %v (continuing)", vmid, strings.TrimSpace(string(out)), err)
	}

	// Convert to a template so `pct clone` can make instant linked (CoW) clones.
	if out, err := exec.Command("pct", "template", fmt.Sprintf("%d", vmid)).CombinedOutput(); err != nil {
		destroy()
		return 0, fmt.Errorf("manager: pct template %d: %s: %w", vmid, strings.TrimSpace(string(out)), err)
	}

	// Record the template VMID so it's reused, and so RemoveImage and the reaper
	// can find it (a referenced VMID is never reaped).
	if rec := m.store.GetImage(imgRec.Ref); rec != nil {
		rec.TemplateVMID = vmid
		if err := m.store.AddImage(rec); err != nil {
			destroy()
			return 0, fmt.Errorf("manager: persist template vmid: %w", err)
		}
	}
	log.Printf("ensureImageTemplateCT: materialized template CT %d for %s", vmid, imgRec.Ref)
	return vmid, nil
}

// createPVELinkedCloneFromTarball provisions a Proxmox CT by linked-cloning the
// image's template CT — an instant copy-on-write clone instead of re-extracting
// the rootfs tarball with `pct create` on every launch. Used on snapshot-capable
// block storages (lvmthin) where the ZFS raw-clone fast path doesn't apply. The
// container is a normal CT, visible and managed via pct exactly like the
// pct-create path. Any failure falls back to that path so a launch never
// hard-fails on the fast path.
func (m *Manager) createPVELinkedCloneFromTarball(id string, imgRec *store.ImageRecord, cfg ContainerConfig) error {
	tmplVMID, err := m.ensureImageTemplateCT(imgRec)
	if err != nil {
		log.Printf("createPVELinkedCloneFromTarball: template prep failed (%v); falling back to pct create", err)
		return m.createPVEFromTarball(id, imgRec, cfg)
	}

	vmid, err := allocateVMID()
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	// Record the VMID up front so gc() doesn't reap the half-built record
	// mid-clone (same rationale as createPVEFromTarball).
	if storeRec := m.store.GetContainer(id); storeRec != nil {
		storeRec.VMID = vmid
		if err := m.store.AddContainer(storeRec); err != nil {
			return fmt.Errorf("manager: persist vmid: %w", err)
		}
	}

	// Fill in LAN config before the IP allocation in finalizePVECT keys off
	// cfg.NetworkMode (may convert --network=host to a LAN NIC).
	applyLANNetworking(&cfg, m.lan, vmid, m.lanMACForContainer(id))

	hostname := id[:12]
	if storeRec := m.store.GetContainer(id); storeRec != nil {
		hostname = storeRec.Name
	}
	hostname = sanitizeHostname(hostname)

	// Linked clone: no --full (copy-on-write) and no --storage (a linked clone
	// must stay on the template's storage; passing --storage is rejected by pct).
	log.Printf("CreateContainer[linked-clone]: pct clone %d → %d for %s", tmplVMID, vmid, id[:12])
	if out, err := exec.Command("pct", "clone", fmt.Sprintf("%d", tmplVMID), fmt.Sprintf("%d", vmid),
		"--hostname", hostname,
	).CombinedOutput(); err != nil {
		log.Printf("createPVELinkedCloneFromTarball: pct clone failed (%s: %v); falling back to pct create", strings.TrimSpace(string(out)), err)
		exec.Command("pct", "destroy", fmt.Sprintf("%d", vmid), "--force").Run()
		return m.createPVEFromTarball(id, imgRec, cfg)
	}

	// The clone inherits the template's image-minimum rootfs; grow it if a larger
	// size was requested (--storage-opt size). Best-effort: a failed grow leaves
	// the image-minimum rootfs rather than failing the launch.
	if want := resolveRootfsGB(cfg, imgRec.TemplateTarball); want > tarballRootfsGB(imgRec.TemplateTarball) {
		if out, err := exec.Command("pct", "resize", fmt.Sprintf("%d", vmid), "rootfs", fmt.Sprintf("%dG", want)).CombinedOutput(); err != nil {
			log.Printf("createPVELinkedCloneFromTarball: resize %d rootfs to %dG: %s: %v (continuing)", vmid, want, strings.TrimSpace(string(out)), err)
		}
	}

	return m.finalizePVECT(id, vmid, hostname, cfg)
}
