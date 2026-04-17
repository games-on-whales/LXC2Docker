// Package lxc wraps go-lxc to provide container lifecycle operations for the
// docker-lxc-daemon. All container names managed here are the raw LXC names
// (which double as Docker container IDs).
package lxc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/image"
	"github.com/games-on-whales/docker-lxc-daemon/internal/oci"
	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
	liblxc "github.com/lxc/go-lxc"
)

// BridgeSpec describes a single LAN bridge known to the daemon. A daemon
// can advertise multiple bridges; containers select one with the
// "dld.bridge=<name>" label.
type BridgeSpec struct {
	Name    string // bridge name (e.g. "vmbr0")
	Prefix  string // IPv4 prefix; VMID becomes last octet (e.g. "192.168.1")
	Gateway string // gateway address (e.g. "192.168.1.1")
	Subnet  int    // prefix length (e.g. 23 for /23)
}

// LANConfig holds daemon-level LAN bridge settings. Bridges is the full
// catalog of known bridges (keyed by name). Default names the bridge used
// when a container requests LAN networking without specifying which
// bridge — empty Default means "no daemon-level LAN".
type LANConfig struct {
	Bridges map[string]BridgeSpec
	Default string
}

// Lookup returns the spec for name, or the default bridge if name is empty.
// Returns ok=false if the requested bridge is unknown.
func (c LANConfig) Lookup(name string) (BridgeSpec, bool) {
	if name == "" {
		name = c.Default
	}
	if name == "" {
		return BridgeSpec{}, false
	}
	spec, ok := c.Bridges[name]
	return spec, ok
}

// Manager owns all LXC operations on behalf of the daemon.
type Manager struct {
	lxcPath    string // e.g. /var/lib/lxc (legacy mode)
	pveStorage string // Proxmox storage name (e.g. "large"); empty = legacy mode
	lan        LANConfig
	store      *store.Store
}

// UsePVE returns true when Proxmox CT mode is active.
func (m *Manager) UsePVE() bool { return m.pveStorage != "" }

// NewManager creates a Manager that stores containers under lxcPath.
// If pveStorage is non-empty, containers are created as Proxmox CTs on
// the named storage (e.g. "large" ZFS pool) and are visible in the
// Proxmox web UI. Otherwise, raw lxc-* commands are used (legacy mode).
func NewManager(lxcPath, pveStorage string, lan LANConfig, st *store.Store) (*Manager, error) {
	if err := os.MkdirAll(lxcPath, 0o755); err != nil {
		return nil, fmt.Errorf("manager: mkdir %s: %w", lxcPath, err)
	}
	if err := EnsureBridge(); err != nil {
		return nil, fmt.Errorf("manager: bridge: %w", err)
	}
	m := &Manager{lxcPath: lxcPath, pveStorage: pveStorage, lan: lan, store: st}
	if pveStorage != "" {
		log.Printf("Proxmox CT mode enabled (storage=%s)", pveStorage)
	}
	if len(lan.Bridges) > 0 {
		for name, b := range lan.Bridges {
			marker := ""
			if name == lan.Default {
				marker = " (default)"
			}
			log.Printf("LAN bridge%s registered: %s prefix=%s gateway=%s /%d",
				marker, b.Name, b.Prefix, b.Gateway, b.Subnet)
		}
	}
	m.reconcile()
	return m, nil
}

// reconcile checks the store against actual LXC state on startup. For
// containers that are still running, it re-applies port forwarding rules
// (which may have been lost if nft state was cleared). For containers
// whose LXC directory no longer exists, it cleans them from the store.
func (m *Manager) reconcile() {
	for _, rec := range m.store.ListContainers() {
		if !m.containerExists(rec.ID) {
			log.Printf("reconcile: removing orphaned store entry %s (%s)", rec.Name, rec.ID[:12])
			m.store.RemoveContainer(rec.ID)
			continue
		}
		state, _ := m.State(rec.ID)
		if state == "running" && rec.IPAddress != "" {
			for _, pb := range rec.PortBindings {
				if err := AddPortForward(rec.IPAddress, pb.HostPort, pb.ContainerPort, pb.Proto); err != nil {
					log.Printf("reconcile: port forward %d->%s:%d/%s failed: %v",
						pb.HostPort, rec.IPAddress, pb.ContainerPort, pb.Proto, err)
				}
			}
			log.Printf("reconcile: container %s (%s) still running, port forwards restored",
				rec.Name, rec.ID[:12])
		}
	}
}

// StartGC launches a background goroutine that periodically reaps stopped
// ephemeral containers created by this daemon.
//
// Safety: the GC ONLY destroys a container when ALL of the following hold:
//   - the store record exists with rec.Ephemeral == true
//   - rec.VMID == 0 (i.e. not a Proxmox CT)
//   - the on-disk LXC config contains EphemeralMarker
//   - the container is in state "exited"
//
// Any record missing any of these checks is left strictly alone. The GC
// never enumerates lxc-ls / pct list — it works only from the store.
// This makes it safe to run on a Proxmox host that contains permanent
// PVE CTs and other LXC containers it did not create.
func (m *Manager) StartGC(ctx context.Context) {
	go func() {
		// Run immediately on startup to clean leftovers, then periodically.
		m.gc()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.gc()
			}
		}
	}()
}

func (m *Manager) gc() {
	for _, rec := range m.store.ListContainers() {
		if !m.isReapable(rec) {
			continue
		}
		state, _ := m.State(rec.ID)
		if state != "exited" {
			continue
		}
		log.Printf("GC: removing stopped ephemeral %s (%s)", rec.Name, rec.ID[:12])
		if rec.IPAddress != "" {
			RemovePortForwards(rec.IPAddress)
		}
		if err := m.RemoveContainer(rec.ID); err != nil {
			log.Printf("GC: failed to remove %s: %v", rec.ID[:12], err)
		}
	}
}

// isReapable returns true only when every safety check confirms the
// container was created by this daemon as ephemeral. Any single failed
// check returns false — and logs why, so unexpected state on the host is
// visible rather than silently destroyed.
func (m *Manager) isReapable(rec *store.ContainerRecord) bool {
	if rec == nil {
		return false
	}
	if !rec.Ephemeral {
		return false
	}
	if rec.VMID != 0 {
		log.Printf("GC: skip %s (%s) — Ephemeral=true but VMID=%d (state inconsistent)",
			rec.Name, rec.ID[:12], rec.VMID)
		return false
	}
	if !m.hasEphemeralMarker(rec.ID) {
		log.Printf("GC: skip %s (%s) — on-disk config missing EphemeralMarker",
			rec.Name, rec.ID[:12])
		return false
	}
	return true
}

// hasEphemeralMarker reports whether the on-disk LXC config for id contains
// the EphemeralMarker comment. This is the second source of truth (the
// store record is the first) — both must agree before the GC will act.
func (m *Manager) hasEphemeralMarker(id string) bool {
	configPath := filepath.Join(m.lxcPath, id, "config")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == EphemeralMarker {
			return true
		}
	}
	return false
}

// PullImage ensures a template container exists for the given image ref.
// For distro images it runs lxc-create with the download template.
// For app images it creates the base template, starts it, installs packages,
// then stops it — producing a ready-to-clone template.
func (m *Manager) PullImage(ref, arch string, progress func(string)) error {
	resolved, err := image.Resolve(ref, arch)
	if err != nil {
		return err
	}

	// If the template container already exists, nothing to do — but restore
	// the store record if it was lost (e.g. state.json was cleared).
	if m.containerExists(resolved.TemplateContainerName) {
		if m.store.GetImage(resolved.Ref) == nil {
			rec := m.restoreImageRecord(resolved)
			if err := m.store.AddImage(rec); err != nil {
				log.Printf("PullImage: warning: could not restore store record for %s: %v", resolved.Ref, err)
			}
		}
		progress("Image already present")
		return nil
	}

	switch resolved.Kind {
	case image.KindDistro:
		return m.pullDistro(resolved, progress)
	case image.KindApp:
		return m.pullApp(resolved, progress)
	case image.KindOCI:
		return m.pullOCI(resolved, progress)
	}
	return fmt.Errorf("manager: unknown image kind")
}

func (m *Manager) pullDistro(r *image.ResolvedImage, progress func(string)) error {
	progress(fmt.Sprintf("Pulling %s %s/%s from images.linuxcontainers.org",
		r.Ref, r.Distro, r.Release))

	c, err := liblxc.NewContainer(r.TemplateContainerName, m.lxcPath)
	if err != nil {
		return fmt.Errorf("manager: new container %s: %w", r.TemplateContainerName, err)
	}

	opts := liblxc.TemplateOptions{
		Template: "download",
		Distro:   r.Distro,
		Release:  r.Release,
		Arch:     r.Arch,
	}
	if err := c.Create(opts); err != nil {
		return fmt.Errorf("manager: create template %s: %w", r.TemplateContainerName, err)
	}

	// Record image in store.
	return m.store.AddImage(&store.ImageRecord{
		ID:           imageID(r.Distro, r.Release),
		Ref:          r.Ref,
		Distro:       r.Distro,
		Release:      r.Release,
		Arch:         r.Arch,
		TemplateName: r.TemplateContainerName,
		Created:      time.Now(),
	})
}

func (m *Manager) pullApp(r *image.ResolvedImage, progress func(string)) error {
	// 1. Ensure the base distro template exists.
	progress(fmt.Sprintf("Pulling base image %s for %s", r.BaseRef, r.Ref))
	baseResolved, err := image.Resolve(r.BaseRef, r.Arch)
	if err != nil {
		return err
	}
	if !m.containerExists(baseResolved.TemplateContainerName) {
		if err := m.pullDistro(baseResolved, progress); err != nil {
			return err
		}
	}

	// 2. Clone base → app template.
	progress(fmt.Sprintf("Creating app template for %s", r.Ref))
	base, err := liblxc.NewContainer(baseResolved.TemplateContainerName, m.lxcPath)
	if err != nil {
		return fmt.Errorf("manager: open base template: %w", err)
	}
	if err := base.Clone(r.TemplateContainerName, liblxc.CloneOptions{
		Backend:  liblxc.Directory,
		Snapshot: false,
	}); err != nil {
		return fmt.Errorf("manager: clone base → app template: %w", err)
	}

	// 3. Rewrite the cloned config to fix AppArmor/userns issues, set up
	//    networking, and write resolv.conf so package installs can resolve DNS.
	//    Use a temporary IP that we free after the build completes.
	templateCfgPath := filepath.Join(m.lxcPath, r.TemplateContainerName, "config")
	templateCfg := ContainerConfig{}
	ip, err := m.store.AllocateIP()
	if err != nil {
		return fmt.Errorf("manager: allocate IP for app template: %w", err)
	}
	defer m.store.FreeIP(ip) // Template doesn't need a permanent IP.

	if err := rewriteConfig(templateCfgPath, &templateCfg, ip, r.TemplateContainerName, false); err != nil {
		return fmt.Errorf("manager: rewrite app template config: %w", err)
	}
	templateRootfs := filepath.Join(m.lxcPath, r.TemplateContainerName, "rootfs")
	resolvPath := filepath.Join(templateRootfs, "etc", "resolv.conf")
	os.Remove(resolvPath)
	os.WriteFile(resolvPath, []byte("nameserver 8.8.8.8\nnameserver 1.1.1.1\n"), 0o644)

	// Start the app template container temporarily.
	appTemplate, err := liblxc.NewContainer(r.TemplateContainerName, m.lxcPath)
	if err != nil {
		return fmt.Errorf("manager: open app template: %w", err)
	}
	if err := appTemplate.Start(); err != nil {
		return fmt.Errorf("manager: start app template: %w", err)
	}
	defer appTemplate.Stop()

	if err := waitRunning(appTemplate, 30*time.Second); err != nil {
		return fmt.Errorf("manager: app template did not start: %w", err)
	}

	// 4. Install packages.
	if len(r.App.Packages) > 0 {
		progress(fmt.Sprintf("Installing packages: %s", strings.Join(r.App.Packages, " ")))
		installCmd := buildInstallCmd(r.Distro, r.App.Packages)
		if err := m.runInContainer(appTemplate, installCmd); err != nil {
			return fmt.Errorf("manager: install packages: %w", err)
		}
	}

	// 5. Run post-install script if any.
	if r.App.PostInstall != "" {
		progress("Running post-install")
		if err := m.runInContainer(appTemplate, r.App.PostInstall); err != nil {
			return fmt.Errorf("manager: post-install: %w", err)
		}
	}
	// Stop is handled by defer above.

	// 7. Record image in store.
	return m.store.AddImage(&store.ImageRecord{
		ID:           imageID(r.Distro, r.Release),
		Ref:          r.Ref,
		Distro:       r.Distro,
		Release:      r.Release,
		Arch:         r.Arch,
		TemplateName: r.TemplateContainerName,
		Created:      time.Now(),
	})
}

// pullOCI pulls an arbitrary OCI/Docker image via skopeo + umoci, unpacks it
// to a rootfs, and creates a template from it. In PVE mode the template is a
// Proxmox CT on the configured storage; otherwise a direct LXC template.
func (m *Manager) pullOCI(r *image.ResolvedImage, progress func(string)) error {
	ociStoreDir := filepath.Join(filepath.Dir(m.lxcPath), "docker-lxc-daemon", "oci")

	cfg, rootfsPath, err := oci.Pull(ociStoreDir, r.Ref, progress)
	if err != nil {
		return fmt.Errorf("manager: oci pull: %w", err)
	}

	var templateVMID int

	if m.UsePVE() {
		// --- Proxmox CT mode ---
		// Create a tarball from the rootfs, then use pct create to make a
		// Proxmox CT template on the configured storage (ZFS).
		progress("Creating Proxmox CT template from OCI rootfs")

		tarball := filepath.Join(os.TempDir(), "oci-template-"+oci.SafeDirName(r.Ref)+".tar.gz")
		defer os.Remove(tarball)

		out, err := exec.Command("tar", "czf", tarball, "-C", rootfsPath, ".").CombinedOutput()
		if err != nil {
			return fmt.Errorf("manager: create tarball: %s: %w", out, err)
		}

		vmid, err := allocateVMID()
		if err != nil {
			return err
		}
		templateVMID = vmid

		hostname := sanitizeHostname("tmpl-" + oci.SafeDirName(r.Ref))

		out, err = exec.Command("pct", "create", fmt.Sprintf("%d", vmid), tarball,
			"--storage", m.pveStorage,
			"--ostype", "unmanaged",
			"--arch", "amd64",
			"--hostname", hostname,
			"--unprivileged", "0",
			"--rootfs", fmt.Sprintf("%s:4", m.pveStorage),
		).CombinedOutput()
		if err != nil {
			return fmt.Errorf("manager: pct create template %d: %s: %w", vmid, out, err)
		}

		// Mark it as a template so it can't be accidentally started.
		exec.Command("pct", "template", fmt.Sprintf("%d", vmid)).Run()

		// Create a ZFS snapshot for instant ephemeral container cloning.
		// pct template converts subvol → basevol, so snapshot the basevol.
		snapDataset := fmt.Sprintf("%s/basevol-%d-disk-0@tmpl", m.pveStorage, vmid)
		if snapOut, snapErr := exec.Command("zfs", "snapshot", snapDataset).CombinedOutput(); snapErr != nil {
			log.Printf("pullOCI: warning: could not create ZFS snapshot %s: %s: %v", snapDataset, snapOut, snapErr)
		} else {
			log.Printf("pullOCI: created ZFS snapshot %s for ephemeral cloning", snapDataset)
		}

		// Write resolv.conf into the template rootfs.
		templateRootfs := m.pveRootfsPath(vmid)
		resolvPath := filepath.Join(templateRootfs, "etc", "resolv.conf")
		os.Remove(resolvPath)
		os.MkdirAll(filepath.Dir(resolvPath), 0o755)
		os.WriteFile(resolvPath, []byte("nameserver 8.8.8.8\nnameserver 1.1.1.1\n"), 0o644)

		// Clean up the OCI working directory.
		os.RemoveAll(rootfsPath)
		oci.Cleanup(ociStoreDir, r.Ref)

		log.Printf("pullOCI: created Proxmox template VMID %d for %s", vmid, r.Ref)
	} else {
		// --- Legacy direct-LXC mode ---
		progress("Creating LXC template from OCI rootfs")
		templateDir := filepath.Join(m.lxcPath, r.TemplateContainerName)
		templateRootfs := filepath.Join(templateDir, "rootfs")
		if err := os.MkdirAll(templateDir, 0o755); err != nil {
			return fmt.Errorf("manager: mkdir template: %w", err)
		}

		if err := os.Rename(rootfsPath, templateRootfs); err != nil {
			out, cpErr := exec.Command("cp", "-a", rootfsPath, templateRootfs).CombinedOutput()
			if cpErr != nil {
				return fmt.Errorf("manager: copy rootfs: %s: %w", out, cpErr)
			}
		}

		minimalConfig := fmt.Sprintf(`lxc.include = /usr/share/lxc/config/common.conf
lxc.arch = linux64
lxc.rootfs.path = dir:%s
lxc.uts.name = %s
`, templateRootfs, r.TemplateContainerName)

		configPath := filepath.Join(templateDir, "config")
		if err := os.WriteFile(configPath, []byte(minimalConfig), 0o644); err != nil {
			return fmt.Errorf("manager: write template config: %w", err)
		}

		resolvPath := filepath.Join(templateRootfs, "etc", "resolv.conf")
		os.Remove(resolvPath)
		os.MkdirAll(filepath.Dir(resolvPath), 0o755)
		os.WriteFile(resolvPath, []byte("nameserver 8.8.8.8\nnameserver 1.1.1.1\n"), 0o644)

		oci.Cleanup(ociStoreDir, r.Ref)

		if data, err := json.Marshal(store.ImageRecord{
			ID:            "oci_" + oci.SafeDirName(r.Ref),
			Ref:           r.Ref,
			Arch:          r.Arch,
			TemplateName:  r.TemplateContainerName,
			OCIEntrypoint: cfg.Entrypoint,
			OCICmd:        cfg.Cmd,
			OCIEnv:        cfg.Env,
			OCIWorkingDir: cfg.WorkingDir,
			OCIPorts:      cfg.Ports,
		}); err == nil {
			os.WriteFile(filepath.Join(templateDir, "oci-meta.json"), data, 0o644)
		}
	}

	progress("Image ready")
	return m.store.AddImage(&store.ImageRecord{
		ID:            "oci_" + oci.SafeDirName(r.Ref),
		Ref:           r.Ref,
		Arch:          r.Arch,
		TemplateName:  r.TemplateContainerName,
		TemplateVMID:  templateVMID,
		Created:       time.Now(),
		OCIEntrypoint: cfg.Entrypoint,
		OCICmd:        cfg.Cmd,
		OCIEnv:        cfg.Env,
		OCIWorkingDir: cfg.WorkingDir,
		OCIPorts:      cfg.Ports,
	})
}

// CreateContainer clones the image template, applies the given config, and
// prepares (but does not start) the container. In PVE mode, containers marked
// with ProxmoxCT are created as full Proxmox CTs (visible in the web UI);
// all others are ephemeral raw-LXC containers with ZFS-cloned rootfs.
func (m *Manager) CreateContainer(id, imageRef string, cfg ContainerConfig) error {
	rec := m.store.GetImage(imageRef)
	if rec == nil {
		return fmt.Errorf("manager: image %q not found; run pull first", imageRef)
	}

	// Translate any per-container ISO requests into bind mounts before
	// dispatching, so all backends (permanent CT / ephemeral / legacy)
	// pick them up via the normal mount path.
	if mounts, err := resolveISOMounts(cfg.ISOs); err != nil {
		return err
	} else {
		cfg.Mounts = append(cfg.Mounts, mounts...)
	}

	if m.UsePVE() && cfg.ProxmoxCT && rec.TemplateVMID > 0 {
		return m.createPVEContainer(id, rec, cfg)
	}
	if m.UsePVE() && rec.TemplateVMID > 0 {
		return m.createEphemeralPVE(id, rec, cfg)
	}
	return m.createLegacyContainer(id, rec, cfg)
}

// resolveISOMounts asks pvesm for the on-host file path of each requested
// ISO volume and returns equivalent read-only bind-mount specs. Used by
// CreateContainer to fold ISO requests into the normal mount processing.
func resolveISOMounts(isos []ISOMount) ([]MountSpec, error) {
	var out []MountSpec
	for _, iso := range isos {
		volid := iso.Storage + ":" + iso.VolumeID
		raw, err := exec.Command("pvesm", "path", volid).Output()
		if err != nil {
			return nil, fmt.Errorf("manager: resolve ISO %s: %w", volid, err)
		}
		host := strings.TrimSpace(string(raw))
		if host == "" {
			return nil, fmt.Errorf("manager: pvesm returned empty path for %s", volid)
		}
		dest := iso.Destination
		if dest == "" {
			dest = "/mnt/" + filepath.Base(iso.VolumeID)
		}
		out = append(out, MountSpec{
			Source:      host,
			Destination: dest,
			ReadOnly:    true,
		})
	}
	return out, nil
}

// createPVEContainer creates a full Proxmox CT via pct clone. The container
// is visible in the Proxmox web UI and managed via pct commands.
func (m *Manager) createPVEContainer(id string, imgRec *store.ImageRecord, cfg ContainerConfig) error {
	vmid, err := allocateVMID()
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}

	// Resolve the LAN bridge per-container (cfg.Bridge selects from the
	// daemon's bridge catalog; empty falls back to the default bridge).
	if cfg.LAN {
		spec, ok := m.lan.Lookup(cfg.Bridge)
		if !ok {
			if cfg.Bridge != "" {
				return fmt.Errorf("manager: container requested bridge %q but daemon has no such bridge configured", cfg.Bridge)
			}
			return fmt.Errorf("manager: container requested LAN networking but daemon has no default bridge configured")
		}
		cfg.LANBridge = spec.Name
		cfg.LANIP = fmt.Sprintf("%s.%d/%d", spec.Prefix, vmid, spec.Subnet)
		cfg.LANGateway = spec.Gateway
		log.Printf("CreateContainer[PVE]: LAN NIC on %s with IP %s", cfg.LANBridge, cfg.LANIP)
	}

	// Resolve target storage: per-container override wins, daemon default otherwise.
	storage := cfg.Storage
	if storage == "" {
		storage = m.pveStorage
	}

	log.Printf("CreateContainer[PVE]: pct clone %d → VMID %d for %s (storage=%s)",
		imgRec.TemplateVMID, vmid, id[:12], storage)
	out, err := exec.Command("pct", "clone",
		fmt.Sprintf("%d", imgRec.TemplateVMID),
		fmt.Sprintf("%d", vmid),
		"--full",
		"--storage", storage,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("manager: pct clone %d → %d: %s: %w", imgRec.TemplateVMID, vmid, out, err)
	}

	// Allocate IP for bridge networking (internal gow0).
	var ip string
	if cfg.NetworkMode != "host" {
		ip, err = m.store.AllocateIP()
		if err != nil {
			exec.Command("pct", "destroy", fmt.Sprintf("%d", vmid), "--force").Run()
			return fmt.Errorf("manager: allocate IP: %w", err)
		}
	}

	// Set console log path.
	cfg.LogFile = LogFilePath(m.lxcPath, id)
	if err := os.MkdirAll(filepath.Dir(cfg.LogFile), 0o755); err != nil {
		return fmt.Errorf("manager: mkdir log dir: %w", err)
	}

	// Determine the container hostname (use Docker name, sanitized for DNS).
	hostname := id[:12]
	if storeRec := m.store.GetContainer(id); storeRec != nil {
		hostname = storeRec.Name
	}
	hostname = sanitizeHostname(hostname)

	// Build rootfs spec for Proxmox config (uses the resolved storage).
	rootfsSpec := fmt.Sprintf("%s:subvol-%d-disk-0,size=4G", storage, vmid)
	rootfsPath := pveRootfsPathOn(storage, vmid)

	// Write the Proxmox CT config.
	if err := writePVEConfig(vmid, hostname, rootfsSpec, rootfsPath, &cfg, ip); err != nil {
		exec.Command("pct", "destroy", fmt.Sprintf("%d", vmid), "--force").Run()
		return fmt.Errorf("manager: write PVE config: %w", err)
	}

	// Prepare rootfs: runtime dirs, resolv.conf.
	rootfs := pveRootfsPathOn(storage, vmid)
	m.prepareRootfs(rootfs, cfg)

	// Update store record with IP, VMID, and storage. Permanent CTs are
	// explicitly non-ephemeral so the GC will never touch them.
	if storeRec := m.store.GetContainer(id); storeRec != nil {
		storeRec.IPAddress = ip
		storeRec.VMID = vmid
		storeRec.Ephemeral = false
		storeRec.Storage = storage
		return m.store.AddContainer(storeRec)
	}
	return nil
}

// createEphemeralPVE creates a raw-LXC container by ZFS-cloning the PVE
// template's rootfs. The container is NOT visible in the Proxmox UI but its
// rootfs lives on the PVE storage pool (ZFS). Note: cfg.Storage cannot
// override the pool here — ZFS clones must live on the same pool as their
// source snapshot. A request for a different storage is honored only by
// permanent CTs (createPVEContainer); for ephemeral we log and proceed.
func (m *Manager) createEphemeralPVE(id string, imgRec *store.ImageRecord, cfg ContainerConfig) error {
	if cfg.Storage != "" && cfg.Storage != m.pveStorage {
		log.Printf("CreateContainer[ephemeral]: ignoring requested storage %q "+
			"(ephemeral containers must use template's pool %q; ZFS clone is pool-local)",
			cfg.Storage, m.pveStorage)
	}
	// ZFS clone the template rootfs for instant provisioning.
	// pct template converts subvol → basevol, so clone from basevol.
	snapDataset := fmt.Sprintf("%s/basevol-%d-disk-0@tmpl", m.pveStorage, imgRec.TemplateVMID)
	cloneDataset := fmt.Sprintf("%s/lxc-%s", m.pveStorage, id)
	cloneMountpoint := fmt.Sprintf("/%s/lxc-%s", m.pveStorage, id)

	log.Printf("CreateContainer[ephemeral]: ZFS clone %s → %s", snapDataset, cloneDataset)
	out, err := exec.Command("zfs", "clone",
		"-o", fmt.Sprintf("mountpoint=%s", cloneMountpoint),
		snapDataset, cloneDataset,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("manager: zfs clone %s → %s: %s: %w", snapDataset, cloneDataset, out, err)
	}

	// Create the LXC config directory.
	containerDir := filepath.Join(m.lxcPath, id)
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		exec.Command("zfs", "destroy", cloneDataset).Run()
		return fmt.Errorf("manager: mkdir %s: %w", containerDir, err)
	}

	// Write a minimal LXC config that references the ZFS clone as rootfs.
	minimalConfig := fmt.Sprintf(`lxc.include = /usr/share/lxc/config/common.conf
lxc.arch = linux64
lxc.rootfs.path = dir:%s
lxc.uts.name = %s
`, cloneMountpoint, id)
	configPath := filepath.Join(containerDir, "config")
	if err := os.WriteFile(configPath, []byte(minimalConfig), 0o644); err != nil {
		exec.Command("zfs", "destroy", cloneDataset).Run()
		return fmt.Errorf("manager: write config: %w", err)
	}

	// Allocate IP for bridge networking.
	var ip string
	if cfg.NetworkMode != "host" {
		ip, err = m.store.AllocateIP()
		if err != nil {
			exec.Command("zfs", "destroy", cloneDataset).Run()
			os.RemoveAll(containerDir)
			return fmt.Errorf("manager: allocate IP: %w", err)
		}
	}

	// Set console log path.
	cfg.LogFile = LogFilePath(m.lxcPath, id)
	if err := os.MkdirAll(filepath.Dir(cfg.LogFile), 0o755); err != nil {
		return fmt.Errorf("manager: mkdir log dir: %w", err)
	}

	// Rewrite config with full daemon-managed settings. Mark as ephemeral
	// so the GC can positively identify this container as reapable.
	// Note: rewriteConfig may populate cfg.SocketLinks for socket bind mounts.
	if err := rewriteConfig(configPath, &cfg, ip, id, true); err != nil {
		return fmt.Errorf("manager: rewrite config: %w", err)
	}

	// Prepare rootfs: runtime dirs, resolv.conf, socket symlinks.
	m.prepareRootfs(cloneMountpoint, cfg)

	// Update store record with IP, mark ephemeral, and record the storage
	// pool so RemoveContainer can locate the ZFS dataset (VMID stays 0).
	if storeRec := m.store.GetContainer(id); storeRec != nil {
		storeRec.IPAddress = ip
		storeRec.Ephemeral = true
		storeRec.Storage = m.pveStorage
		return m.store.AddContainer(storeRec)
	}
	return nil
}

// createLegacyContainer clones via lxc-copy (no Proxmox, no ZFS).
func (m *Manager) createLegacyContainer(id string, imgRec *store.ImageRecord, cfg ContainerConfig) error {
	log.Printf("CreateContainer[legacy]: cloning %s → %s", imgRec.TemplateName, id)
	out, err := exec.Command("lxc-copy",
		"-n", imgRec.TemplateName,
		"-N", id,
		"--lxcpath", m.lxcPath,
		"--newpath", m.lxcPath,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("manager: clone %s → %s: %s: %w", imgRec.TemplateName, id, out, err)
	}

	// Allocate IP for bridge networking.
	var ip string
	if cfg.NetworkMode != "host" {
		ip, err = m.store.AllocateIP()
		if err != nil {
			m.destroyOrphan(id)
			return fmt.Errorf("manager: allocate IP: %w", err)
		}
	}

	// Set console log path.
	cfg.LogFile = LogFilePath(m.lxcPath, id)
	if err := os.MkdirAll(filepath.Dir(cfg.LogFile), 0o755); err != nil {
		return fmt.Errorf("manager: mkdir log dir: %w", err)
	}

	// Rewrite the cloned config. Legacy raw-LXC containers are always
	// ephemeral (no PVE UI presence), so mark them for GC eligibility.
	configPath := filepath.Join(m.lxcPath, id, "config")
	if err := rewriteConfig(configPath, &cfg, ip, id, true); err != nil {
		return fmt.Errorf("manager: rewrite config: %w", err)
	}

	// Prepare rootfs.
	rootfs := filepath.Join(m.lxcPath, id, "rootfs")
	m.prepareRootfs(rootfs, cfg)

	// Update store record with IP and mark ephemeral.
	if storeRec := m.store.GetContainer(id); storeRec != nil {
		storeRec.IPAddress = ip
		storeRec.Ephemeral = true
		return m.store.AddContainer(storeRec)
	}
	return nil
}

// prepareRootfs ensures runtime directories and resolv.conf exist in the rootfs.
func (m *Manager) prepareRootfs(rootfs string, cfg ContainerConfig) {
	// Ensure runtime directories referenced by XDG_RUNTIME_DIR.
	for _, e := range cfg.Env {
		if strings.HasPrefix(e, "XDG_RUNTIME_DIR=") {
			dir := strings.TrimPrefix(e, "XDG_RUNTIME_DIR=")
			if dir != "" {
				runtimeDir := filepath.Join(rootfs, dir)
				if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
					log.Printf("prepareRootfs: warning: mkdir XDG_RUNTIME_DIR %s: %v", runtimeDir, err)
				}
			}
		}
	}

	// Ensure resolv.conf for DNS resolution.
	resolvPath := filepath.Join(rootfs, "etc", "resolv.conf")
	os.Remove(resolvPath)
	os.MkdirAll(filepath.Dir(resolvPath), 0o755)
	if err := os.WriteFile(resolvPath, []byte("nameserver 8.8.8.8\nnameserver 1.1.1.1\n"), 0o644); err != nil {
		log.Printf("prepareRootfs: warning: write resolv.conf: %v", err)
	}

	// Create symlinks for socket bind-mounts. Socket mounts use a directory
	// mount instead of a file mount (see appendSocketMount), so the
	// application needs a symlink from the expected path to the socket
	// inside the mounted directory.
	for dest, target := range cfg.SocketLinks {
		linkPath := filepath.Join(rootfs, strings.TrimPrefix(dest, "/"))
		// Follow symlinks in the link's parent directory within the rootfs.
		// E.g. /var/run → /run in many container images.
		linkDir := filepath.Dir(linkPath)
		if resolved, err := resolveInRootfs(rootfs, filepath.Dir(dest)); err == nil {
			linkDir = filepath.Join(rootfs, resolved)
		}
		linkPath = filepath.Join(linkDir, filepath.Base(dest))

		os.MkdirAll(linkDir, 0o755)
		os.Remove(linkPath) // remove any existing file/symlink
		if err := os.Symlink(target, linkPath); err != nil {
			log.Printf("prepareRootfs: warning: symlink %s → %s: %v", linkPath, target, err)
		}
	}
}

// StartContainer starts a stopped container.
// For Proxmox CTs (VMID > 0), uses pct start; otherwise lxc-start.
func (m *Manager) StartContainer(id string) error {
	state, _ := m.State(id)
	if state == "running" {
		return nil
	}

	rec := m.store.GetContainer(id)
	if rec != nil && rec.VMID > 0 {
		return m.startPVEContainer(id, rec.VMID)
	}
	return m.startLXCContainer(id)
}

func (m *Manager) startPVEContainer(id string, vmid int) error {
	log.Printf("StartContainer[PVE]: pct start %d (%s)", vmid, id[:12])
	out, err := exec.Command("pct", "start", fmt.Sprintf("%d", vmid)).CombinedOutput()
	if err != nil {
		// Dump config for debugging.
		if cfgData, readErr := os.ReadFile(pveConfigPath(vmid)); readErr == nil {
			log.Printf("StartContainer[PVE]: FAILED config for VMID %d:\n%s", vmid, cfgData)
		}
		return fmt.Errorf("manager: pct start %d: %s: %w", vmid, out, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		state, _ := m.State(id)
		if state == "running" {
			log.Printf("StartContainer[PVE]: VMID %d (%s) is running", vmid, id[:12])
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("manager: VMID %d did not reach RUNNING within 30s", vmid)
}

func (m *Manager) startLXCContainer(id string) error {
	log.Printf("StartContainer[LXC]: starting %s", id)
	out, err := exec.Command("lxc-start", "-n", id, "--lxcpath", m.lxcPath,
		"--logfile", filepath.Join(m.lxcPath, id, "lxc-start.log"),
		"--logpriority", "DEBUG").CombinedOutput()
	if err != nil {
		if cfgData, readErr := os.ReadFile(filepath.Join(m.lxcPath, id, "config")); readErr == nil {
			log.Printf("StartContainer[LXC]: FAILED config for %s:\n%s", id, cfgData)
		}
		if logData, readErr := os.ReadFile(filepath.Join(m.lxcPath, id, "lxc-start.log")); readErr == nil {
			log.Printf("StartContainer[LXC]: lxc-start log for %s:\n%s", id, logData)
		}
		return fmt.Errorf("manager: start %s: %s: %w", id, out, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		state, _ := m.State(id)
		if state == "running" {
			log.Printf("StartContainer[LXC]: %s is running", id)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("manager: container %s did not reach RUNNING within 30s", id)
}

// StopContainer stops a running container gracefully, waiting up to timeout.
// For Proxmox CTs uses pct shutdown; otherwise lxc-stop.
func (m *Manager) StopContainer(id string, timeout time.Duration) error {
	state, _ := m.State(id)
	if state != "running" {
		return nil
	}

	if rec := m.store.GetContainer(id); rec != nil && rec.VMID > 0 {
		out, err := exec.Command("pct", "shutdown",
			fmt.Sprintf("%d", rec.VMID),
			"--timeout", fmt.Sprintf("%d", int(timeout.Seconds())),
		).CombinedOutput()
		if err != nil {
			// Fall back to forced stop.
			out2, err2 := exec.Command("pct", "stop", fmt.Sprintf("%d", rec.VMID)).CombinedOutput()
			if err2 != nil {
				return fmt.Errorf("manager: pct stop %d: %s (shutdown: %s): %w", rec.VMID, out2, out, err2)
			}
		}
		return nil
	}

	out, err := exec.Command("lxc-stop", "-n", id, "--lxcpath", m.lxcPath,
		"-t", fmt.Sprintf("%d", int(timeout.Seconds()))).CombinedOutput()
	if err != nil {
		return fmt.Errorf("manager: stop %s: %s: %w", id, out, err)
	}
	return nil
}

// KillContainer sends a signal to the container's init process. For SIGKILL
// it uses pct stop (PVE) or lxc-stop --kill; for other signals it sends them
// directly to the container's init PID.
func (m *Manager) KillContainer(id, signal string) error {
	if signal == "" {
		signal = "KILL"
	}

	rec := m.store.GetContainer(id)

	if signal == "KILL" || signal == "9" || signal == "SIGKILL" {
		if rec != nil && rec.VMID > 0 {
			out, err := exec.Command("pct", "stop", fmt.Sprintf("%d", rec.VMID)).CombinedOutput()
			if err != nil {
				return fmt.Errorf("manager: pct stop %d: %s: %w", rec.VMID, out, err)
			}
			return nil
		}
		out, err := exec.Command("lxc-stop", "--kill", "-n", id, "--lxcpath", m.lxcPath).
			CombinedOutput()
		if err != nil {
			return fmt.Errorf("manager: kill %s: %s: %w", id, out, err)
		}
		return nil
	}

	// For other signals, get the init PID and send the signal directly.
	// Works for both PVE and raw LXC containers since the init PID is
	// visible on the host either way.
	var pidOut []byte
	var err error
	if rec != nil && rec.VMID > 0 {
		// For PVE containers, lxc-info works with the VMID as name
		// using the default lxcpath (/var/lib/lxc).
		pidOut, err = exec.Command("lxc-info", "-n", fmt.Sprintf("%d", rec.VMID), "-pH").Output()
	} else {
		pidOut, err = exec.Command("lxc-info", "-n", id, "--lxcpath", m.lxcPath, "-pH").Output()
	}
	if err != nil {
		return fmt.Errorf("manager: kill %s: cannot get init pid: %w", id, err)
	}
	pidStr := strings.TrimSpace(string(pidOut))
	if pidStr == "" || pidStr == "-1" {
		return fmt.Errorf("manager: kill %s: container not running (no init pid)", id)
	}
	killOut, err := exec.Command("kill", fmt.Sprintf("-%s", signal), pidStr).CombinedOutput()
	if err != nil {
		return fmt.Errorf("manager: kill %s (pid %s, signal %s): %s: %w", id, pidStr, signal, killOut, err)
	}
	return nil
}

// RemoveContainer destroys a container and removes it from the store.
// Routes to pct destroy (PVE CT), ZFS destroy (ephemeral PVE), or
// lxc-destroy (legacy) based on container type.
func (m *Manager) RemoveContainer(id string) error {
	state, _ := m.State(id)
	if state == "running" {
		return fmt.Errorf("manager: cannot remove running container %s; stop it first", id)
	}

	rec := m.store.GetContainer(id)

	if rec != nil && rec.VMID > 0 {
		// Proxmox CT — destroy via pct.
		out, err := exec.Command("pct", "destroy", fmt.Sprintf("%d", rec.VMID), "--force").CombinedOutput()
		if err != nil {
			return fmt.Errorf("manager: pct destroy %d: %s: %w", rec.VMID, out, err)
		}
		return m.store.RemoveContainer(id)
	}

	if m.UsePVE() {
		// Ephemeral container with ZFS-cloned rootfs — destroy the ZFS
		// dataset, then remove the LXC config directory. Use the storage
		// recorded at create time when available so this works even if
		// the daemon's default storage has since changed.
		storage := m.pveStorage
		if rec != nil && rec.Storage != "" {
			storage = rec.Storage
		}
		cloneDataset := fmt.Sprintf("%s/lxc-%s", storage, id)
		out, err := exec.Command("zfs", "destroy", cloneDataset).CombinedOutput()
		if err != nil {
			log.Printf("RemoveContainer: zfs destroy %s: %s: %v (continuing)", cloneDataset, out, err)
		}
		containerDir := filepath.Join(m.lxcPath, id)
		os.RemoveAll(containerDir)
		return m.store.RemoveContainer(id)
	}

	// Legacy: lxc-destroy.
	out, err := exec.Command("lxc-destroy", "-n", id, "--lxcpath", m.lxcPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("manager: destroy %s: %s: %w", id, out, err)
	}
	return m.store.RemoveContainer(id)
}

// RemoveImage destroys the template container and removes the image record.
func (m *Manager) RemoveImage(ref string) error {
	rec := m.store.GetImage(ref)
	if rec == nil {
		return fmt.Errorf("manager: image %q not found", ref)
	}

	if rec.TemplateVMID > 0 {
		// PVE template — first destroy any ZFS snapshots (used by ephemeral
		// clones), then destroy the CT template via pct.
		snapDataset := fmt.Sprintf("%s/basevol-%d-disk-0@tmpl", m.pveStorage, rec.TemplateVMID)
		exec.Command("zfs", "destroy", snapDataset).Run() // best-effort
		out, err := exec.Command("pct", "destroy", fmt.Sprintf("%d", rec.TemplateVMID), "--force").CombinedOutput()
		if err != nil {
			return fmt.Errorf("manager: pct destroy template %d: %s: %w", rec.TemplateVMID, out, err)
		}
		return m.store.RemoveImage(ref)
	}

	// Legacy template — lxc-destroy.
	if m.containerExists(rec.TemplateName) {
		out, err := exec.Command("lxc-destroy", "-n", rec.TemplateName, "--lxcpath", m.lxcPath).CombinedOutput()
		if err != nil {
			return fmt.Errorf("manager: destroy template %s: %s: %w", rec.TemplateName, out, err)
		}
	}
	return m.store.RemoveImage(ref)
}

// State returns the Docker-compatible state string for a container.
// For PVE CTs uses pct status; otherwise lxc-info.
func (m *Manager) State(id string) (string, error) {
	if rec := m.store.GetContainer(id); rec != nil && rec.VMID > 0 {
		out, err := exec.Command("pct", "status", fmt.Sprintf("%d", rec.VMID)).Output()
		if err != nil {
			return "exited", nil
		}
		// pct status output: "status: running" or "status: stopped"
		status := strings.TrimSpace(string(out))
		status = strings.TrimPrefix(status, "status: ")
		switch status {
		case "running":
			return "running", nil
		case "stopped":
			return "exited", nil
		default:
			return status, nil
		}
	}

	out, err := exec.Command("lxc-info", "-n", id, "--lxcpath", m.lxcPath, "-sH").Output()
	if err != nil {
		return "exited", nil
	}
	lxcState := strings.ToLower(strings.TrimSpace(string(out)))
	switch lxcState {
	case "running":
		return "running", nil
	case "stopped":
		return "exited", nil
	default:
		return lxcState, nil
	}
}

// Exec runs cmd inside the container. For PVE CTs uses pct exec;
// otherwise lxc-attach. Returns an *exec.Cmd not yet started.
func (m *Manager) Exec(id string, cmd []string, env []string) *exec.Cmd {
	if rec := m.store.GetContainer(id); rec != nil && rec.VMID > 0 {
		args := []string{"exec", fmt.Sprintf("%d", rec.VMID), "--"}
		args = append(args, cmd...)
		c := exec.Command("pct", args...)
		c.Env = env
		return c
	}
	args := []string{"-n", id, "--lxcpath", m.lxcPath, "--"}
	args = append(args, cmd...)
	c := exec.Command("lxc-attach", args...)
	c.Env = env
	return c
}

// LogPath returns the console log file path for a container.
func (m *Manager) LogPath(id string) string {
	return LogFilePath(m.lxcPath, id)
}

// LXCPath returns the container storage root.
func (m *Manager) LXCPath() string { return m.lxcPath }

// RootfsPath returns the rootfs path for a container.
// For PVE CTs returns the ZFS subvol path; otherwise the lxcpath rootfs.
func (m *Manager) RootfsPath(id string) string {
	if rec := m.store.GetContainer(id); rec != nil && rec.VMID > 0 {
		return m.pveRootfsPath(rec.VMID)
	}
	// For ephemeral PVE containers, the rootfs is a ZFS clone mounted
	// directly. Check if it exists before falling back to lxcpath/rootfs.
	if m.UsePVE() {
		clonePath := fmt.Sprintf("/%s/lxc-%s", m.pveStorage, id)
		if fi, err := os.Stat(clonePath); err == nil && fi.IsDir() {
			return clonePath
		}
	}
	return filepath.Join(m.lxcPath, id, "rootfs")
}

// --- helpers ---

// sanitizeHostname converts a string to a valid DNS hostname: lowercase,
// only letters/digits/hyphens, max 63 chars, no leading/trailing hyphens.
func sanitizeHostname(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	h := b.String()
	// Collapse multiple hyphens.
	for strings.Contains(h, "--") {
		h = strings.ReplaceAll(h, "--", "-")
	}
	h = strings.Trim(h, "-")
	if len(h) > 63 {
		h = h[:63]
	}
	h = strings.TrimRight(h, "-")
	if h == "" {
		h = "ct"
	}
	return h
}

// allocateVMID requests the next available Proxmox VMID.
func allocateVMID() (int, error) {
	out, err := exec.Command("pvesh", "get", "/cluster/nextid").Output()
	if err != nil {
		return 0, fmt.Errorf("allocate VMID: %w", err)
	}
	var vmid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &vmid); err != nil {
		return 0, fmt.Errorf("parse VMID %q: %w", string(out), err)
	}
	return vmid, nil
}

// pveRootfsPath returns the rootfs path for a Proxmox CT on the daemon's
// default ZFS storage. Prefer pveRootfsPathOn when the storage is known
// per-container (since PVE CTs may be cloned to a non-default storage).
func (m *Manager) pveRootfsPath(vmid int) string {
	return pveRootfsPathOn(m.pveStorage, vmid)
}

// pveRootfsPathOn returns the rootfs path for a Proxmox CT on the named
// ZFS storage pool. For pool "large" + VMID 260: /large/subvol-260-disk-0.
func pveRootfsPathOn(storage string, vmid int) string {
	return fmt.Sprintf("/%s/subvol-%d-disk-0", storage, vmid)
}

// pveConfigPath returns the Proxmox config path for a VMID.
func pveConfigPath(vmid int) string {
	return fmt.Sprintf("/etc/pve/lxc/%d.conf", vmid)
}

// destroyOrphan removes a cloned container that failed during CreateContainer.
func (m *Manager) destroyOrphan(id string) {
	if rec := m.store.GetContainer(id); rec != nil && rec.VMID > 0 {
		exec.Command("pct", "destroy", fmt.Sprintf("%d", rec.VMID), "--force").Run()
		return
	}
	if m.UsePVE() {
		// Ephemeral ZFS clone.
		cloneDataset := fmt.Sprintf("%s/lxc-%s", m.pveStorage, id)
		exec.Command("zfs", "destroy", cloneDataset).Run()
		os.RemoveAll(filepath.Join(m.lxcPath, id))
		return
	}
	exec.Command("lxc-destroy", "-n", id, "--lxcpath", m.lxcPath).Run()
}

func (m *Manager) containerExists(name string) bool {
	// Check store for PVE container by ID.
	if rec := m.store.GetContainer(name); rec != nil && rec.VMID > 0 {
		_, err := os.Stat(pveConfigPath(rec.VMID))
		return err == nil
	}
	// Check image records for PVE template by name.
	for _, img := range m.store.ListImages() {
		if img.TemplateName == name && img.TemplateVMID > 0 {
			_, err := os.Stat(pveConfigPath(img.TemplateVMID))
			return err == nil
		}
	}
	// Raw LXC container — check lxcPath.
	configPath := filepath.Join(m.lxcPath, name, "config")
	_, err := os.Stat(configPath)
	return err == nil
}

func waitRunning(c *liblxc.Container, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.State() == liblxc.RUNNING {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("container %s did not reach RUNNING within %s", c.Name(), timeout)
}

func (m *Manager) runInContainer(c *liblxc.Container, shellCmd string) error {
	out, err := exec.Command(
		"lxc-attach", "-n", c.Name(), "--lxcpath", m.lxcPath,
		"--", "/bin/sh", "-c", shellCmd,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", out, err)
	}
	return nil
}

func buildInstallCmd(distro string, packages []string) string {
	pkgs := strings.Join(packages, " ")
	switch distro {
	case "alpine":
		return fmt.Sprintf("apk add --no-cache %s", pkgs)
	case "fedora", "centos", "rockylinux", "almalinux":
		return fmt.Sprintf("dnf install -y %s", pkgs)
	case "archlinux":
		return fmt.Sprintf("pacman -Sy --noconfirm %s", pkgs)
	default: // debian, ubuntu, etc.
		return fmt.Sprintf("apt-get update && apt-get install -y --no-install-recommends %s", pkgs)
	}
}

func imageID(distro, release string) string {
	return distro + "_" + release
}

// restoreImageRecord reconstructs a store.ImageRecord for a template that
// exists on disk but whose store entry was lost. For OCI images it reads the
// oci-meta.json sidecar written at pull time; for distro/app images it
// reconstructs from the resolved image metadata.
func (m *Manager) restoreImageRecord(resolved *image.ResolvedImage) *store.ImageRecord {
	if resolved.Kind == image.KindOCI {
		// Try sidecar file first.
		sidecar := filepath.Join(m.lxcPath, resolved.TemplateContainerName, "oci-meta.json")
		if data, err := os.ReadFile(sidecar); err == nil {
			var rec store.ImageRecord
			if json.Unmarshal(data, &rec) == nil {
				rec.Created = time.Now()
				return &rec
			}
		}
		// Fallback: minimal record without OCI metadata.
		return &store.ImageRecord{
			ID:           "oci_" + oci.SafeDirName(resolved.Ref),
			Ref:          resolved.Ref,
			Arch:         resolved.Arch,
			TemplateName: resolved.TemplateContainerName,
			Created:      time.Now(),
		}
	}
	return &store.ImageRecord{
		ID:           imageID(resolved.Distro, resolved.Release),
		Ref:          resolved.Ref,
		Distro:       resolved.Distro,
		Release:      resolved.Release,
		Arch:         resolved.Arch,
		TemplateName: resolved.TemplateContainerName,
		Created:      time.Now(),
	}
}
