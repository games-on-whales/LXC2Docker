// Package lxc wraps LXC lifecycle operations for the docker-lxc-daemon. All
// container names managed here are the raw LXC names (which double as Docker
// container IDs).
package lxc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/image"
	"github.com/games-on-whales/LXC2Docker/internal/oci"
	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// LANConfig holds daemon-level LAN bridge settings for dual-NIC containers.
type LANConfig struct {
	Bridge  string // physical bridge name (e.g. "vmbr0"); empty = disabled
	Prefix  string // IP prefix (e.g. "192.168.1"); VMID becomes last octet
	Gateway string // LAN gateway (e.g. "192.168.1.1")
	Subnet  int    // prefix length (e.g. 23 for /23)
}

// Manager owns all LXC operations on behalf of the daemon.
type Manager struct {
	lxcPath    string // e.g. /var/lib/lxc (legacy mode)
	pveStorage string // Proxmox storage name (e.g. "large"); empty = legacy mode
	pveStgType string // backend type of pveStorage: "zfspool", "lvmthin", "dir", …
	lan        LANConfig
	store      *store.Store
	// tmplMu serializes ZFS image-template materialization so two concurrent
	// launches of the same image don't race to create the template dataset.
	tmplMu sync.Mutex
	// minFreeBytes is the low-space threshold for the create pre-flight and the
	// disk-pressure watcher. Zero disables both. Set via SetMinFreeBytes.
	minFreeBytes uint64
	// cacheDir holds bulky regenerable data (tarballs, OCI, volumes); empty
	// means use the state dir. Set via SetCacheDir. See CacheDir.
	cacheDir string

	// DefaultMemoryBytes is the RAM (in bytes) given to PVE CTs that don't
	// request an explicit --memory or dld.memory label. 0 means "use the
	// host's total RAM" (no artificial cap). Set from the --default-memory
	// flag after construction.
	DefaultMemoryBytes int64
}

// UsePVE returns true when Proxmox CT mode is active.
func (m *Manager) UsePVE() bool { return m.pveStorage != "" }

// PVEStorage returns the configured Proxmox storage name, or "" when the
// daemon isn't in PVE mode. Used by the API layer's size-of-image logic to
// build the ZFS dataset name for `zfs get used`.
func (m *Manager) PVEStorage() string { return m.pveStorage }

// CacheDir is where the daemon keeps bulky, regenerable data — image rootfs
// tarballs, OCI unpacks, and named-volume backing — as opposed to the small
// JSON metadata under the state dir. Defaults to the state dir; relocate it
// off the (often small) host root with --cache-path or the PVE auto-default.
func (m *Manager) CacheDir() string {
	if m.cacheDir == "" {
		return m.store.RootDir()
	}
	return m.cacheDir
}

// SetCacheDir resolves and applies the bulky-cache location from the operator's
// --cache-path value (empty = auto). On a directory-capable PVE storage (a ZFS
// pool, mounted at /<pool>) an empty value defaults the cache onto that pool so
// it never fills the host root; block storages (lvmthin/lvm) have no POSIX
// mountpoint, so it stays on the state dir and we log a recommendation.
func (m *Manager) SetCacheDir(explicit string) {
	m.cacheDir = resolveCacheDir(explicit, m.store.RootDir(), m.pveStorage, m.pveStgType)
	if m.cacheDir == m.store.RootDir() {
		if m.UsePVE() && explicit == "" && !m.pveStorageIsZFS() {
			log.Printf("cache: bulky image/volume data stays under %s (host root); "+
				"set --cache-path to a large filesystem to keep it off root", m.store.RootDir())
		}
		return
	}
	if err := os.MkdirAll(m.cacheDir, 0o755); err != nil {
		log.Printf("cache: mkdir %s: %v; falling back to %s", m.cacheDir, err, m.store.RootDir())
		m.cacheDir = ""
		return
	}
	log.Printf("cache: bulky image/volume data stored under %s", m.cacheDir)
}

// resolveCacheDir is the pure path-selection rule behind SetCacheDir.
func resolveCacheDir(explicit, statePath, pveStorage, pveStgType string) string {
	if explicit != "" {
		return explicit
	}
	if pveStgType == "zfspool" && pveStorage != "" {
		return filepath.Join("/"+pveStorage, "docker-lxc-daemon-cache")
	}
	return statePath
}

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
		m.pveStgType = detectPVEStorageType(pveStorage)
		log.Printf("Proxmox CT mode enabled (storage=%s, type=%s)", pveStorage, m.pveStgType)
	}
	if lan.Bridge != "" {
		log.Printf("LAN bridge enabled (bridge=%s, prefix=%s, gateway=%s, /%d)",
			lan.Bridge, lan.Prefix, lan.Gateway, lan.Subnet)
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
			if rec.VMID == 0 {
				if err := m.ensureBridgeAttachment(rec.ID); err != nil {
					log.Printf("reconcile: bridge attach for %s (%s) failed: %v", rec.Name, rec.ID[:12], err)
				}
			}
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

// StartGC launches a background goroutine that periodically removes stopped
// ephemeral containers. Compose-managed services (those with Docker Compose
// labels) and Proxmox CTs (VMID > 0) are left alone. This handles the common
// case where Wolf sessions end abnormally (e.g. daemon restart) and child
// containers (PulseAudio, Steam, Wolf-UI) are left behind.
func (m *Manager) StartGC(ctx context.Context) {
	go func() {
		// Run immediately on startup to clean leftovers, then periodically.
		m.gc()
		m.reapOrphanCTs()
		m.reapOrphanTarballs()
		m.rotateLogs()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.gc()
				m.reapOrphanCTs()
				m.reapOrphanTarballs()
				m.rotateLogs()
			}
		}
	}()
}

// StartNetworkReconciler keeps the managed bridge and NAT table present even
// if another host firewall manager reloads nftables after daemon startup.
func (m *Manager) StartNetworkReconciler(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := EnsureBridge(); err != nil {
					log.Printf("network reconcile: %v", err)
				}
				// Keep every container's managed /etc/hosts block current
				// so a peer that drifted to a new bridge IP stays
				// resolvable by name without recreating the dependent.
				m.syncHosts()
			}
		}
	}()
}

// consoleLogMax caps each container's console.log at this size. LXC opens
// the file with O_APPEND, so truncating in place works correctly — the next
// write seeks to the new end-of-file (0) without corruption. 10 MB keeps
// the log viewer responsive without losing more than a few minutes of
// output for chatty containers.
const consoleLogMax = 10 * 1024 * 1024

// rotateLogs enforces consoleLogMax on every container's console log. Over
// the cap, we copy the tail to <log>.1 and truncate the live file. This
// keeps the log viewer snappy and bounds disk usage without disrupting the
// running LXC process.
func (m *Manager) rotateLogs() {
	for _, rec := range m.store.ListContainers() {
		logPath := LogFilePath(m.lxcPath, rec.ID)
		fi, err := os.Stat(logPath)
		if err != nil || fi.Size() <= consoleLogMax {
			continue
		}
		// Preserve the last half of the cap as .1 so the log viewer can
		// still show the most recent backlog after rotation.
		if err := copyTail(logPath, logPath+".1", consoleLogMax/2); err != nil {
			log.Printf("rotateLogs: copyTail %s: %v", rec.ID[:12], err)
		}
		if err := os.Truncate(logPath, 0); err != nil {
			log.Printf("rotateLogs: truncate %s: %v", rec.ID[:12], err)
		}
	}
}

// copyTail copies the last n bytes of src to dst. Used by log rotation.
func copyTail(src, dst string, n int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if fi.Size() > n {
		if _, err := in.Seek(fi.Size()-n, 0); err != nil {
			return err
		}
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// HealthEmitter reports each probe outcome so the API layer can publish a
// Docker "health_status" event. May be nil.
type HealthEmitter func(id, status string)

// StartHealthWatcher runs configured HEALTHCHECKs on their Interval. It
// ticks once per second and skips containers whose next-check deadline
// hasn't arrived. Probes use mgr.Exec (lxc-attach / pct exec) so they run
// inside the container, matching Docker's semantics.
func (m *Manager) StartHealthWatcher(ctx context.Context, emit HealthEmitter) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				m.runDueHealthchecks(now, emit)
			}
		}
	}()
}

func (m *Manager) runDueHealthchecks(now time.Time, emit HealthEmitter) {
	for _, rec := range m.store.ListContainers() {
		if len(rec.HealthcheckTest) == 0 {
			continue
		}
		// Honor NONE disable form: ["NONE"] means "no healthcheck".
		if len(rec.HealthcheckTest) == 1 && rec.HealthcheckTest[0] == "NONE" {
			continue
		}
		if state, _ := m.State(rec.ID); state != "running" {
			// Reset to "starting" when the container restarts.
			if rec.HealthStatus != "starting" {
				rec.HealthStatus = "starting"
				rec.HealthFailingStreak = 0
				m.store.AddContainer(rec)
			}
			continue
		}
		interval := time.Duration(rec.HealthcheckInterval)
		if interval <= 0 {
			interval = 30 * time.Second // Docker default.
		}
		if rec.HealthLastCheck != nil && now.Sub(*rec.HealthLastCheck) < interval {
			continue
		}
		m.runOneHealthcheck(rec, now, emit)
	}
}

// runOneHealthcheck runs a single HEALTHCHECK probe and updates the
// container record with the outcome. Health status flips to "healthy"
// after any success and to "unhealthy" once the failing streak exceeds
// Retries (Docker default: 3).
func (m *Manager) runOneHealthcheck(rec *store.ContainerRecord, start time.Time, emit HealthEmitter) {
	test := rec.HealthcheckTest
	// Test formats:
	//   ["CMD", "bin", "arg1", ...]        — exec argv directly
	//   ["CMD-SHELL", "<shell string>"]    — run via /bin/sh -c
	//   ["NONE"]                           — disabled (handled upstream)
	var cmdArgs []string
	switch test[0] {
	case "CMD":
		cmdArgs = test[1:]
	case "CMD-SHELL":
		if len(test) < 2 {
			return
		}
		cmdArgs = []string{"/bin/sh", "-c", test[1]}
	default:
		// Bare list; treat as argv.
		cmdArgs = test
	}
	if len(cmdArgs) == 0 {
		return
	}

	timeout := time.Duration(rec.HealthcheckTimeout)
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := m.Exec(rec.ID, cmdArgs, nil)
	// Bind the command to the timeout context so it's killed if the probe
	// overruns (exec.Command doesn't honor context by default — wrap via
	// CommandContext).
	cmdCtx := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	out, err := cmdCtx.CombinedOutput()
	end := time.Now()

	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}

	prevStatus := rec.HealthStatus
	result := store.HealthResult{
		Start:    start,
		End:      end,
		ExitCode: exitCode,
		Output:   truncateOutput(string(out)),
	}
	rec.HealthLastCheck = &end
	rec.HealthLog = append(rec.HealthLog, result)
	// Keep the last 5 entries, matching Docker's default.
	if len(rec.HealthLog) > 5 {
		rec.HealthLog = rec.HealthLog[len(rec.HealthLog)-5:]
	}
	retries := rec.HealthcheckRetries
	if retries <= 0 {
		retries = 3
	}
	if exitCode == 0 {
		rec.HealthFailingStreak = 0
		rec.HealthStatus = "healthy"
	} else {
		// During the start period (default 0), failures don't count against
		// the streak — they're recorded but don't flip the status. Once
		// past the grace window, normal Retries-based logic applies. This
		// matches Docker's --health-start-period semantics.
		inStartPeriod := false
		if rec.StartedAt != nil && rec.HealthcheckStartPeriod > 0 {
			if end.Sub(*rec.StartedAt) < time.Duration(rec.HealthcheckStartPeriod) {
				inStartPeriod = true
			}
		}
		if !inStartPeriod {
			rec.HealthFailingStreak++
			if rec.HealthFailingStreak >= retries {
				rec.HealthStatus = "unhealthy"
			}
		}
	}
	if err := m.store.AddContainer(rec); err != nil {
		log.Printf("health-watcher: persist %s: %v", rec.ID[:12], err)
		return
	}
	if emit != nil && rec.HealthStatus != prevStatus {
		emit(rec.ID, rec.HealthStatus)
	}
}

// truncateOutput clips probe stdout/stderr to a reasonable length so the
// health log doesn't balloon the state file. Docker's limit is 4096; we
// use the same.
func truncateOutput(s string) string {
	const max = 4096
	if len(s) > max {
		return s[:max]
	}
	return s
}

// StartRestartWatcher enforces HostConfig.RestartPolicy and HostConfig.AutoRemove
// on container exits. Polling is cheap — State() runs lxc-info / pct status
// which are sub-millisecond per container — so we check every 5 seconds. A
// dedicated watcher (vs folding into gc()) keeps the faster cadence for
// restart events decoupled from the slower gc cycle.
type RestartEmitter func(id, action string)

func (m *Manager) StartRestartWatcher(ctx context.Context) {
	m.StartRestartWatcherWithEmitter(ctx, nil)
}

func (m *Manager) StartRestartWatcherWithEmitter(ctx context.Context, emit RestartEmitter) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.enforceRestartPolicies(emit)
			}
		}
	}()
}

// enforceRestartPolicies walks stored containers once per tick. For each
// container that has exited, it consults RestartPolicy/AutoRemove and takes
// the appropriate action (restart, remove, or leave).
func (m *Manager) enforceRestartPolicies(emit RestartEmitter) {
	for _, rec := range m.store.ListContainers() {
		// Skip containers that were never started — "created" state shouldn't
		// trigger restart logic.
		if rec.StartedAt == nil {
			continue
		}
		state, _ := m.State(rec.ID)
		if state != "exited" {
			continue
		}

		// Record the first observed exit time so inspect can show
		// "Exited X minutes ago". The watcher is the earliest place we
		// can reliably detect a spontaneous exit — StopContainer sets
		// FinishedAt on user-initiated stops, but crashes need this path.
		if rec.FinishedAt == nil {
			now := time.Now()
			rec.FinishedAt = &now
			if err := m.store.AddContainer(rec); err != nil {
				log.Printf("restart-watcher: persist FinishedAt %s: %v", rec.ID[:12], err)
			}
		}

		// AutoRemove wins over RestartPolicy because Docker treats
		// --rm as a stronger signal — the container ceases to exist
		// after exit regardless of what the policy says.
		if rec.AutoRemove {
			log.Printf("restart-watcher: auto-removing exited container %s (%s)", rec.Name, rec.ID[:12])
			RemovePortForwards(rec.IPAddress)
			if err := m.RemoveContainer(rec.ID); err != nil {
				log.Printf("restart-watcher: remove %s: %v", rec.ID[:12], err)
			} else if emit != nil {
				emit(rec.ID, "destroy")
			}
			continue
		}

		if !shouldRestart(rec) {
			continue
		}

		log.Printf("restart-watcher: restarting %s (%s) per policy=%s (attempt %d)",
			rec.Name, rec.ID[:12], rec.RestartPolicy, rec.RestartCount+1)
		if err := m.StartContainer(rec.ID); err != nil {
			log.Printf("restart-watcher: start %s: %v", rec.ID[:12], err)
			continue
		}
		rec.RestartCount++
		if err := m.store.AddContainer(rec); err != nil {
			log.Printf("restart-watcher: persist %s: %v", rec.ID[:12], err)
		}
		if emit != nil {
			emit(rec.ID, "restart")
		}
	}
}

// shouldRestart reports whether a stopped container should be auto-restarted
// based on its stored RestartPolicy + StoppedByUser flag. The semantics
// match the Docker daemon:
//   - ""/"no"         → never
//   - "always"        → always, even if the user stopped it
//   - "unless-stopped"→ restart unless StoppedByUser is set
//   - "on-failure"    → restart up to MaximumRetryCount times; we can't
//     distinguish "failure" from "clean exit" without an exit code (LXC's
//     State() doesn't expose one), so we treat every exit as failure. The
//     retry cap prevents infinite loops for containers that exit
//     immediately.
func shouldRestart(rec *store.ContainerRecord) bool {
	switch rec.RestartPolicy {
	case "always":
		return true
	case "unless-stopped":
		return !rec.StoppedByUser
	case "on-failure":
		if rec.RestartMaxRetry > 0 && rec.RestartCount >= rec.RestartMaxRetry {
			return false
		}
		return !rec.StoppedByUser
	default:
		return false
	}
}

func (m *Manager) gc() {
	// Separate ephemeral containers into stopped (remove immediately) and
	// running (check for orphans).
	var stopped []*store.ContainerRecord
	var runningSession []*store.ContainerRecord // Wolf-UI, WolfSteam, etc.
	var runningSupport []*store.ContainerRecord // WolfPulseAudio, etc.

	for _, rec := range m.store.ListContainers() {
		// Never touch Proxmox CTs or compose-managed services.
		if rec.VMID > 0 {
			continue
		}
		if rec.Labels != nil {
			if _, ok := rec.Labels["com.docker.compose.service"]; ok {
				continue
			}
		}
		if smoothNASManagedContainer(rec) {
			continue
		}
		// Never touch transient image-build containers. The classic builder runs
		// each RUN step inside an ephemeral "build-<id>" LXC container; without
		// this the GC classifies it as an orphaned support container (no "_" in
		// the name, no session container present) and kills it mid-build —
		// SIGKILL surfaces to the build as "RUN failed: exit status 137". The
		// builder is torn down by the build flow itself (finalize / cleanupTemp).
		if strings.HasPrefix(rec.ID, "build-") || strings.HasPrefix(rec.Name, "build-") {
			continue
		}

		state, _ := m.State(rec.ID)
		if state == "exited" {
			// A container that has never been started also reads as "exited"
			// (lxc-info returns nothing for a not-yet-running or still-being-
			// created CT). It is really in "created" state: a `docker run` is
			// mid-flight between the create and start calls, and a slow image
			// clone (e.g. a freshly built image whose template dataset has to be
			// materialized first) can stretch that window to several seconds.
			// Reaping it here deletes the container out from under the imminent
			// attach/start, which then 404s. Only ephemeral containers that have
			// actually run and exited are eligible for GC — this mirrors
			// enforceRestartPolicies, which already skips never-started CTs.
			if rec.StartedAt == nil {
				continue
			}
			stopped = append(stopped, rec)
		} else if state == "running" {
			if smoothNASRunnerWorker(rec) {
				continue
			}
			// Session containers have unique per-session names (Wolf-UI_<id>,
			// WolfSteam_<id>). Support containers are generic (WolfPulseAudio).
			if strings.Contains(rec.Name, "_") {
				runningSession = append(runningSession, rec)
			} else {
				runningSupport = append(runningSupport, rec)
			}
		}
	}

	// Remove all stopped ephemeral containers.
	for _, rec := range stopped {
		log.Printf("GC: removing stopped container %s (%s)", rec.Name, rec.ID[:12])
		if rec.IPAddress != "" {
			RemovePortForwards(rec.IPAddress)
		}
		if err := m.RemoveContainer(rec.ID); err != nil {
			log.Printf("GC: failed to remove %s: %v", rec.ID[:12], err)
		}
	}

	// Orphan detection: if there are support containers (PulseAudio) but
	// no session containers AND no running owner, the support containers
	// are orphans from sessions that ended abnormally.
	//
	// A long-running owner spawns support containers (PulseAudio) via the
	// Docker API and keeps them for the lifetime of the owner, even while
	// idle (before any session). We must not reap them while the owner runs.
	// Two owner deployment models exist:
	//   - a Proxmox CT (VMID > 0), e.g. Wolf running as a PVE CT; and
	//   - a SmoothNAS-managed container (io.smoothnas.managed=true), e.g.
	//     Wolf running as a managed LXC2Docker container rather than a PVE
	//     CT. That same label already exempts the owner itself from GC above.
	if len(runningSession) == 0 && len(runningSupport) > 0 {
		ownerRunning := false
		for _, rec := range m.store.ListContainers() {
			isOwner := rec.VMID > 0 || smoothNASManagedContainer(rec)
			if !isOwner {
				continue
			}
			if st, _ := m.State(rec.ID); st == "running" {
				ownerRunning = true
				break
			}
		}
		if !ownerRunning {
			for _, rec := range runningSupport {
				log.Printf("GC: stopping orphaned container %s (%s, image=%s)",
					rec.Name, rec.ID[:12], rec.Image)
				if err := m.StopContainer(rec.ID, 5*time.Second); err != nil {
					log.Printf("GC: failed to stop %s: %v", rec.ID[:12], err)
					continue
				}
				if rec.IPAddress != "" {
					RemovePortForwards(rec.IPAddress)
				}
				if err := m.RemoveContainer(rec.ID); err != nil {
					log.Printf("GC: failed to remove %s: %v", rec.ID[:12], err)
				}
			}
		}
	}
}

func smoothNASManagedContainer(rec *store.ContainerRecord) bool {
	if rec == nil || rec.Labels == nil {
		return false
	}
	return rec.Labels["io.smoothnas.managed"] == "true"
}

func smoothNASRunnerWorker(rec *store.ContainerRecord) bool {
	if rec == nil || rec.Labels == nil {
		return false
	}
	return rec.Labels["io.smoothnas.gh-runner.worker"] == "true"
}

// PullOpts controls a PullImage invocation. Credentials (if non-empty) are
// passed to skopeo as --src-creds so private registries succeed. OnEvent
// receives structured layer-progress events so the API layer can stream
// Docker-style pull progress to Portainer.
type PullOpts struct {
	Credentials string
	OnStatus    func(string)
	OnEvent     func(oci.ProgressEvent)
}

// PullImage ensures a template container exists for the given image ref,
// using only a status callback. Thin wrapper around PullImageWith kept for
// internal callers that don't care about credentials or structured events.
func (m *Manager) PullImage(ref, arch string, progress func(string)) error {
	return m.PullImageWith(ref, arch, PullOpts{OnStatus: progress})
}

// ociRemoteDigest resolves a ref's current registry manifest digest (the
// arch-specific child digest, comparable to what pullOCI records). It is a
// package variable so tests can stub the registry round-trip; in production it
// points at oci.RemoteDigest (a metadata-only `skopeo inspect`).
var ociRemoteDigest = oci.RemoteDigest

// isDigestPinnedRef reports whether ref pins an immutable content digest
// (e.g. "repo@sha256:…"), as opposed to a mutable tag like ":latest". A
// digest-pinned ref always resolves to the same bytes, so a cached template
// for one is never stale.
func isDigestPinnedRef(ref string) bool {
	return strings.Contains(ref, "@")
}

// PullImageWith is the full-fat version of PullImage. OCI pulls honor
// opts.Credentials (sent to skopeo) and emit layer progress via
// opts.OnEvent. Distro and app pulls ignore credentials — they're fetched
// from images.linuxcontainers.org which is public.
func (m *Manager) PullImageWith(ref, arch string, opts PullOpts) error {
	resolved, err := image.Resolve(ref, arch, m.UsePVE())
	if err != nil {
		return err
	}
	// Legacy shim — downstream code still expects a single status callback.
	progress := opts.OnStatus
	if progress == nil {
		progress = func(string) {}
	}

	// If the template container already exists, decide whether it can be
	// reused or has gone stale. A digest-pinned ref ("repo@sha256:…") is
	// immutable, and distro/app templates are keyed by distro+release — for
	// those, "present" means "up to date". A mutable OCI tag (e.g. ":latest")
	// can drift, so consult the registry and only reuse the cached template
	// when the remote manifest digest still matches what we pulled. Without
	// this check a ":latest" template, once created, is NEVER refreshed.
	if m.containerExists(resolved.TemplateContainerName) {
		rec := m.store.GetImage(resolved.Ref)
		if rec == nil {
			// Restore a store record lost out from under us (e.g. state.json
			// was cleared) so the rest of the daemon can find the template.
			rec = m.restoreImageRecord(resolved)
			if err := m.store.AddImage(rec); err != nil {
				log.Printf("PullImage: warning: could not restore store record for %s: %v", resolved.Ref, err)
			}
		}

		if resolved.Kind != image.KindOCI || isDigestPinnedRef(resolved.Ref) {
			progress("Image already present")
			return nil
		}

		remoteDigest, derr := ociRemoteDigest(resolved.Ref, arch, opts.Credentials)
		switch {
		case derr != nil:
			// Registry unreachable (offline / transient error): keep the
			// working template rather than destroying a cache we can't replace.
			log.Printf("PullImage: remote digest check for %s failed (%v); reusing local template", resolved.Ref, derr)
			progress("Image already present (registry unreachable)")
			return nil
		case remoteDigest != "" && rec.RepoDigest == remoteDigest:
			progress(fmt.Sprintf("Status: Image is up to date for %s", resolved.Ref))
			return nil
		default:
			// The mutable tag points at a new digest (or we have no recorded
			// digest to compare): drop the stale template so the pull below
			// rebuilds it. Running containers are independent rootfs copies
			// (cp -a) and are unaffected by destroying the clone source.
			log.Printf("PullImage: %s is stale (local=%q remote=%q); re-pulling", resolved.Ref, rec.RepoDigest, remoteDigest)
			if err := m.RemoveImage(resolved.Ref); err != nil {
				return fmt.Errorf("manager: replace stale template for %s: %w", resolved.Ref, err)
			}
		}
	}

	switch resolved.Kind {
	case image.KindDistro:
		return m.pullDistro(resolved, progress)
	case image.KindApp:
		return m.pullApp(resolved, progress)
	case image.KindOCI:
		return m.pullOCI(resolved, opts)
	}
	return fmt.Errorf("manager: unknown image kind")
}

func (m *Manager) pullDistro(r *image.ResolvedImage, progress func(string)) error {
	progress(fmt.Sprintf("Pulling %s %s/%s from images.linuxcontainers.org",
		r.Ref, r.Distro, r.Release))

	out, err := exec.Command(
		"lxc-create",
		"-n", r.TemplateContainerName,
		"--lxcpath", m.lxcPath,
		"-t", "download",
		"--",
		"-d", r.Distro,
		"-r", r.Release,
		"-a", r.Arch,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("manager: create template %s: %s: %w", r.TemplateContainerName, out, err)
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
	baseResolved, err := image.Resolve(r.BaseRef, r.Arch, m.UsePVE())
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
	if err := m.cloneLegacyTemplate(baseResolved.TemplateContainerName, r.TemplateContainerName); err != nil {
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
	os.WriteFile(resolvPath, []byte(defaultResolvConf()), 0o644)

	// Start the app template container temporarily.
	out, err := exec.Command("lxc-start", "-n", r.TemplateContainerName, "--lxcpath", m.lxcPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("manager: start app template: %s: %w", out, err)
	}
	defer exec.Command("lxc-stop", "-n", r.TemplateContainerName, "--lxcpath", m.lxcPath).Run()

	if err := m.waitRunning(r.TemplateContainerName, 30*time.Second); err != nil {
		return fmt.Errorf("manager: app template did not start: %w", err)
	}

	// 4. Install packages.
	if len(r.App.Packages) > 0 {
		progress(fmt.Sprintf("Installing packages: %s", strings.Join(r.App.Packages, " ")))
		installCmd := buildInstallCmd(r.Distro, r.App.Packages)
		if err := m.runInContainer(r.TemplateContainerName, installCmd); err != nil {
			return fmt.Errorf("manager: install packages: %w", err)
		}
	}

	// 5. Run post-install script if any.
	if r.App.PostInstall != "" {
		progress("Running post-install")
		if err := m.runInContainer(r.TemplateContainerName, r.App.PostInstall); err != nil {
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
func (m *Manager) pullOCI(r *image.ResolvedImage, opts PullOpts) error {
	ociStoreDir := filepath.Join(m.CacheDir(), "oci")

	progress := opts.OnStatus
	if progress == nil {
		progress = func(string) {}
	}
	cfg, rootfsPath, err := oci.Pull(ociStoreDir, r.Ref, oci.PullOpts{
		Credentials: opts.Credentials,
		OnStatus:    opts.OnStatus,
		OnEvent:     opts.OnEvent,
	})
	if err != nil {
		return fmt.Errorf("manager: oci pull: %w", err)
	}

	// Record the REGISTRY source digest as RepoDigest — NOT cfg.Digest, which
	// is the digest skopeo writes into the local OCI layout. skopeo converts
	// Docker schema2 manifests to OCI media types on copy, so the layout digest
	// differs from the registry's; storing it would make the staleness check in
	// PullImageWith compare unlike values and re-pull the full image on every
	// materialise even when the tag never moved. RemoteDigest is the same call
	// that check uses, so an unchanged tag now compares equal and reuses the
	// cached template. Best-effort: an empty digest just forces one re-pull next
	// time (correct, only slower). It also makes the RepoDigests we report match
	// what `docker inspect` shows (the registry digest).
	repoDigest, derr := ociRemoteDigest(r.Ref, r.Arch, opts.Credentials)
	if derr != nil || repoDigest == "" {
		log.Printf("pullOCI: could not record registry digest for %s: %v (will re-pull next materialise)", r.Ref, derr)
	}

	var templateVMID int
	var templateTarball string

	if m.UsePVE() {
		// --- Proxmox CT mode ---
		// Store the rootfs as a tarball; containers are `pct create`d from it
		// (storage-agnostic, and no Proxmox template CT clutters the web UI).
		progress("Storing image rootfs")

		tarball := m.pveTemplateTarballPath(r.Ref)
		os.MkdirAll(filepath.Dir(tarball), 0o755)
		os.Remove(tarball) // clear any stale tarball on re-pull

		if out, err := exec.Command("tar", "czf", tarball, "-C", rootfsPath, ".").CombinedOutput(); err != nil {
			return fmt.Errorf("manager: tar rootfs: %s: %w", out, err)
		}
		templateTarball = tarball

		// Clean up the OCI working directory.
		os.RemoveAll(rootfsPath)
		oci.Cleanup(ociStoreDir, r.Ref)

		log.Printf("pullOCI: stored rootfs tarball %s for %s", tarball, r.Ref)
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
`, templateRootfs, sanitizeHostname("tmpl-"+oci.SafeDirName(r.Ref)))

		configPath := filepath.Join(templateDir, "config")
		if err := os.WriteFile(configPath, []byte(minimalConfig), 0o644); err != nil {
			return fmt.Errorf("manager: write template config: %w", err)
		}

		resolvPath := filepath.Join(templateRootfs, "etc", "resolv.conf")
		os.Remove(resolvPath)
		os.MkdirAll(filepath.Dir(resolvPath), 0o755)
		os.WriteFile(resolvPath, []byte(defaultResolvConf()), 0o644)

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
			OCILabels:     cfg.Labels,
			RepoDigest:    repoDigest,
		}); err == nil {
			os.WriteFile(filepath.Join(templateDir, "oci-meta.json"), data, 0o644)
		}
	}

	progress("Image ready")
	imgRec := &store.ImageRecord{
		ID:              "oci_" + oci.SafeDirName(r.Ref),
		Ref:             r.Ref,
		Arch:            r.Arch,
		TemplateName:    r.TemplateContainerName,
		TemplateVMID:    templateVMID,
		TemplateTarball: templateTarball,
		Created:         time.Now(),
		OCIEntrypoint:   cfg.Entrypoint,
		OCICmd:          cfg.Cmd,
		OCIEnv:          cfg.Env,
		OCIWorkingDir:   cfg.WorkingDir,
		OCIPorts:        cfg.Ports,
		OCILabels:       cfg.Labels,
		RepoDigest:      repoDigest,
	}
	if err := m.store.AddImage(imgRec); err != nil {
		return err
	}
	// Eagerly materialize the copy-on-write template now, off the pull's critical
	// path, so the one-time ~15s rootfs extraction never lands on a user's first
	// launch — the first open becomes a ~2s clone like every subsequent one.
	m.prewarmImageTemplate(imgRec)
	return nil
}

// prewarmImageTemplate kicks off (asynchronously, best-effort) the one-time
// materialization of an image's clone template, so the first container launch
// pays only the fast clone instead of the tarball extraction. No-op for non-PVE
// mode and for tarball-less images, and for storages with no clone template
// (dir/lvm/nfs re-extract per launch — nothing to pre-build). Failures are
// logged and simply mean the first launch materializes lazily as before.
func (m *Manager) prewarmImageTemplate(imgRec *store.ImageRecord) {
	if !m.UsePVE() || imgRec == nil || imgRec.TemplateTarball == "" {
		return
	}
	if !m.pveStorageIsZFS() && !m.pveStorageSupportsLinkedClone() {
		return
	}
	go func() {
		if m.pveStorageIsZFS() {
			if _, err := m.ensureImageTemplateDataset(imgRec); err != nil {
				log.Printf("prewarmImageTemplate: %s: %v (will materialize on first launch)", imgRec.Ref, err)
			}
			return
		}
		if _, err := m.ensureImageTemplateCT(imgRec); err != nil {
			log.Printf("prewarmImageTemplate: %s: %v (will materialize on first launch)", imgRec.Ref, err)
		}
	}()
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
	if err := m.checkCreateDiskPressure(cfg); err != nil {
		return err
	}
	if cfg.NetworkMode != "host" {
		if err := EnsureBridge(); err != nil {
			return fmt.Errorf("manager: bridge: %w", err)
		}
	}

	// Warm reuse: adopt the warm rootfs of a previous session's CT (same
	// name+image, now stopped) instead of cloning a fresh one. Only the CT config
	// is rewritten for the new session — no clone, and the in-rootfs warm state
	// survives. Falls back to a normal create if the CT has gone missing.
	if cfg.ReuseVMID > 0 {
		if err := m.reconfigureReusedPVECT(id, cfg); err != nil {
			log.Printf("CreateContainer[reuse]: adopt VMID %d for %s failed (%v); cloning fresh", cfg.ReuseVMID, shortID(id), err)
			cfg.ReuseVMID = 0
		} else {
			return nil
		}
	}

	// New images store a rootfs tarball and are created storage-agnostically
	// via `pct create` (no Proxmox template CT — templates stay out of the UI).
	if m.UsePVE() && rec.TemplateTarball != "" {
		// Fast path: provision by copy-on-write cloning a once-materialized
		// template instead of re-extracting the tarball on every launch —
		// near-instant, the bulk of the Wolf-app launch latency.
		//   - ZFS: raw `zfs clone` of a hidden template dataset (also yields a
		//     raw-LXC container), skipped for UI-visible Proxmox CTs (gow.pve).
		//   - lvmthin: linked `pct clone` of a template CT, a normal Proxmox CT.
		// Any failure falls back to the storage-agnostic pct-create path.
		if m.pveStorageIsZFS() && !cfg.ProxmoxCT {
			return m.createZFSCloneFromTarball(id, rec, cfg)
		}
		if m.pveStorageSupportsLinkedClone() {
			return m.createPVELinkedCloneFromTarball(id, rec, cfg)
		}
		return m.createPVEFromTarball(id, rec, cfg)
	}
	// Backward compatibility: images pulled before the tarball change still use
	// the VMID-based pct-template clone paths.
	if m.UsePVE() && cfg.ProxmoxCT && rec.TemplateVMID > 0 {
		return m.createPVEContainer(id, rec, cfg)
	}
	if m.UsePVE() && rec.TemplateVMID > 0 {
		return m.createEphemeralPVE(id, rec, cfg)
	}
	return m.createLegacyContainer(id, rec, cfg)
}

// createPVEContainer creates a full Proxmox CT via pct clone. The container
// is visible in the Proxmox web UI and managed via pct commands.
// writeContainerIDHostname writes hostname to <statepath>/containers/<id>/hostname
// and returns that path, to be bind-mounted at /etc/hostname. The path embeds the
// container ID so apps can identify their own container from /proc/self/mountinfo
// (matching Docker's …/containers/<id>/hostname bind).
func (m *Manager) writeContainerIDHostname(id, hostname string) (string, error) {
	dir := filepath.Join(m.store.RootDir(), "containers", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "hostname")
	if err := os.WriteFile(p, []byte(hostname+"\n"), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

func (m *Manager) lanMACForContainer(id string) string {
	mac := stableLANMac(id)
	rec := m.store.GetContainer(id)
	if rec == nil {
		return mac
	}
	if strings.TrimSpace(rec.LANMacAddress) != "" {
		return strings.TrimSpace(rec.LANMacAddress)
	}
	rec.LANMacAddress = mac
	if err := m.store.AddContainer(rec); err != nil {
		log.Printf("lan mac: persist %s for %s: %v", mac, id[:12], err)
	}
	return mac
}

func (m *Manager) createPVEContainer(id string, imgRec *store.ImageRecord, cfg ContainerConfig) error {
	vmid, err := allocateVMID()
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}

	// Fill in LAN config from daemon settings before any networking setup.
	// This may also convert a --network=host container into LAN mode (a CT
	// can't share the host netns), so it must run before the IP allocation
	// below keys off cfg.NetworkMode.
	applyLANNetworking(&cfg, m.lan, vmid, m.lanMACForContainer(id))

	log.Printf("CreateContainer[PVE]: pct clone %d → VMID %d for %s", imgRec.TemplateVMID, vmid, id[:12])
	out, err := exec.Command("pct", "clone",
		fmt.Sprintf("%d", imgRec.TemplateVMID),
		fmt.Sprintf("%d", vmid),
		"--full",
		"--storage", m.pveStorage,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("manager: pct clone %d → %d: %s: %w", imgRec.TemplateVMID, vmid, out, err)
	}

	// Allocate IP for bridge networking on the internal managed bridge.
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

	// Docker-compat: bind /etc/hostname from a path embedding the container ID
	// so apps can identify their own container via /proc/self/mountinfo.
	if src, err := m.writeContainerIDHostname(id, hostname); err != nil {
		log.Printf("createPVEContainer: container hostname for %s: %v", id[:12], err)
	} else {
		cfg.IDHostnameSource = src
	}

	// Build rootfs spec for Proxmox config. The clone inherits the template's
	// volume size; an explicit DiskSizeGB is recorded in the spec so a later
	// `pct resize` / config read reflects the requested size.
	sizeGB := 4
	if cfg.DiskSizeGB > 0 {
		sizeGB = cfg.DiskSizeGB
	}
	rootfsSpec := fmt.Sprintf("%s:subvol-%d-disk-0,size=%dG", m.pveStorage, vmid, sizeGB)
	rootfsPath := m.pveRootfsPath(vmid)

	// Write the Proxmox CT config.
	if err := writePVEConfig(vmid, hostname, rootfsSpec, rootfsPath, &cfg, ip, m.DefaultMemoryBytes); err != nil {
		exec.Command("pct", "destroy", fmt.Sprintf("%d", vmid), "--force").Run()
		return fmt.Errorf("manager: write PVE config: %w", err)
	}

	// Prepare rootfs: runtime dirs, resolv.conf.
	rootfs := m.pveRootfsPath(vmid)
	m.prepareRootfs(rootfs, cfg)

	// Update store record with IP and VMID.
	if storeRec := m.store.GetContainer(id); storeRec != nil {
		storeRec.IPAddress = ip
		storeRec.VMID = vmid
		return m.store.AddContainer(storeRec)
	}
	return nil
}

// createEphemeralPVE creates a raw-LXC container by ZFS-cloning the PVE
// template's rootfs. The container is NOT visible in the Proxmox UI but its
// rootfs lives on the PVE storage pool (ZFS).
func (m *Manager) createEphemeralPVE(id string, imgRec *store.ImageRecord, cfg ContainerConfig) error {
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

	return m.finalizeRawZFSClone(id, cloneDataset, cloneMountpoint, cfg)
}

// finalizeRawZFSClone turns a freshly-created ZFS clone (already mounted at
// cloneMountpoint) into a started-able raw-LXC container: it writes the LXC
// config, allocates an IP, rewrites the config with daemon-managed settings,
// and prepares the rootfs. The container stays invisible to the Proxmox UI
// (VMID 0); RemoveContainer cleans up the `<pool>/lxc-<id>` dataset. Shared by
// createEphemeralPVE (clone from a PVE template CT) and
// createZFSCloneFromTarball (clone from a tarball-materialized template).
func (m *Manager) finalizeRawZFSClone(id, cloneDataset, cloneMountpoint string, cfg ContainerConfig) error {
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
		var err error
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

	// Rewrite config with full daemon-managed settings.
	// Note: rewriteConfig may populate cfg.SocketLinks for socket bind mounts.
	if err := rewriteConfig(configPath, &cfg, ip, id, true); err != nil {
		return fmt.Errorf("manager: rewrite config: %w", err)
	}

	// Prepare rootfs: runtime dirs, resolv.conf, socket symlinks.
	m.prepareRootfs(cloneMountpoint, cfg)

	// Update store record with IP (VMID stays 0 for ephemeral).
	if storeRec := m.store.GetContainer(id); storeRec != nil {
		storeRec.IPAddress = ip
		return m.store.AddContainer(storeRec)
	}
	return nil
}

// createLegacyContainer clones the image template into a new container by
// directory copy (no Proxmox, no ZFS).
//
// We deliberately avoid `lxc-copy`: its directory-backed storage clone rsyncs
// through the new mount API (move_detached_mount), which newer kernels (e.g.
// Proxmox 7.0-pve) deny in this context — so it slowly rsyncs and then fails.
// A plain rootfs copy works regardless of the kernel's mount-API mediation.
func (m *Manager) createLegacyContainer(id string, imgRec *store.ImageRecord, cfg ContainerConfig) error {
	log.Printf("CreateContainer[legacy]: cloning %s → %s", imgRec.TemplateName, id)
	if err := m.cloneLegacyTemplate(imgRec.TemplateName, id); err != nil {
		return err
	}

	// Allocate IP for bridge networking.
	var ip string
	if cfg.NetworkMode != "host" {
		var err error
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

	// Rewrite the cloned config.
	configPath := filepath.Join(m.lxcPath, id, "config")
	if err := rewriteConfig(configPath, &cfg, ip, id, true); err != nil {
		return fmt.Errorf("manager: rewrite config: %w", err)
	}

	// Prepare rootfs.
	rootfs := filepath.Join(m.lxcPath, id, "rootfs")
	m.prepareRootfs(rootfs, cfg)

	// Update store record with IP.
	if storeRec := m.store.GetContainer(id); storeRec != nil {
		storeRec.IPAddress = ip
		return m.store.AddContainer(storeRec)
	}
	return nil
}

func (m *Manager) cloneLegacyTemplate(templateName, id string) error {
	// Clone by directory copy rather than lxc-copy. lxc-copy's directory-backed
	// storage clone rsyncs through the new mount API (move_detached_mount),
	// which newer kernels (Proxmox 7.0-pve / AppArmor 4.1) deny in the daemon's
	// context — so it slowly rsyncs and then fails, hanging container creation.
	// A plain rootfs copy works regardless of the kernel's mount-API mediation.
	if err := m.cloneLegacyTemplateByCopy(templateName, id); err != nil {
		return fmt.Errorf("manager: clone %s → %s: %w", templateName, id, err)
	}
	return nil
}

func (m *Manager) cloneLegacyTemplateByCopy(templateName, id string) error {
	templateRootfs := filepath.Join(m.lxcPath, templateName, "rootfs")
	if info, err := os.Stat(templateRootfs); err != nil {
		return fmt.Errorf("stat template rootfs %s: %w", templateRootfs, err)
	} else if !info.IsDir() {
		return fmt.Errorf("template rootfs %s is not a directory", templateRootfs)
	}

	containerDir := filepath.Join(m.lxcPath, id)
	containerRootfs := filepath.Join(containerDir, "rootfs")
	if err := os.RemoveAll(containerDir); err != nil {
		return fmt.Errorf("remove partial container dir %s: %w", containerDir, err)
	}
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		return fmt.Errorf("mkdir container dir %s: %w", containerDir, err)
	}

	out, err := exec.Command("cp", "-a", templateRootfs, containerRootfs).CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(containerDir)
		return fmt.Errorf("copy rootfs %s → %s: %s: %w", templateRootfs, containerRootfs, out, err)
	}

	minimalConfig := fmt.Sprintf(`lxc.include = /usr/share/lxc/config/common.conf
lxc.arch = linux64
lxc.rootfs.path = dir:%s
lxc.uts.name = %s
`, containerRootfs, sanitizeHostname(id))
	if err := os.WriteFile(filepath.Join(containerDir, "config"), []byte(minimalConfig), 0o644); err != nil {
		_ = os.RemoveAll(containerDir)
		return fmt.Errorf("write fallback config: %w", err)
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
				ensureRuntimeDir(rootfs, dir)
			}
		}
	}

	resolvPath := filepath.Join(rootfs, "etc", "resolv.conf")
	os.Remove(resolvPath)
	os.MkdirAll(filepath.Dir(resolvPath), 0o755)
	if err := os.WriteFile(resolvPath, []byte(buildResolvConf(cfg)), 0o644); err != nil {
		log.Printf("prepareRootfs: warning: write resolv.conf: %v", err)
	}

	// Apply Docker's --add-host semantics by appending to /etc/hosts. We
	// preserve any content the image shipped with so we don't wipe out
	// distro-provided entries like "127.0.0.1 localhost".
	if len(cfg.ExtraHosts) > 0 {
		hostsPath := filepath.Join(rootfs, "etc", "hosts")
		existing, _ := os.ReadFile(hostsPath)
		var extra strings.Builder
		if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
			extra.WriteByte('\n')
		}
		extra.WriteString("# docker-lxc-daemon: --add-host entries\n")
		for _, h := range cfg.ExtraHosts {
			// Docker's format is "name:ip" (yes, name first). Accept either.
			name, ip, ok := strings.Cut(h, ":")
			if !ok {
				continue
			}
			name = strings.TrimSpace(name)
			ip = strings.TrimSpace(ip)
			if name == "" || ip == "" {
				continue
			}
			// /etc/hosts format is "<ip> <hostname>" (ip first); swap.
			extra.WriteString(ip)
			extra.WriteByte(' ')
			extra.WriteString(name)
			extra.WriteByte('\n')
		}
		combined := append(existing, []byte(extra.String())...)
		if err := os.WriteFile(hostsPath, combined, 0o644); err != nil {
			log.Printf("prepareRootfs: warning: write /etc/hosts: %v", err)
		}
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

// ensureRuntimeDir creates the container's XDG_RUNTIME_DIR and makes its parent
// chain traversable by non-root users.
//
// The XDG spec mandates the runtime dir itself be 0700, but its *parents* must
// stay traversable. A plain os.MkdirAll(dir, 0o700) applied 0700 to every
// intermediate it created, so a deep XDG_RUNTIME_DIR (e.g. a host path mirrored
// into the container, such as Wolf's /var/lib/smoothnas/plugins/wolf/runtime)
// left intermediate dirs as 0700 root. A non-root run-user (e.g. gow's "retro",
// uid 1000) then could not traverse them to reach the bind-mounted wayland
// socket, so the nested compositor died with "Could not connect to remote
// display: Permission denied".
//
// We add the execute bit (traversal only, never read/write) to each parent
// component under the rootfs — this also repairs components that already exist
// at a restrictive mode (which MkdirAll leaves untouched) — and create the leaf
// at the spec-mandated 0700.
func ensureRuntimeDir(rootfs, dir string) {
	leaf := filepath.Join(rootfs, dir)
	parent := filepath.Dir(leaf)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		log.Printf("prepareRootfs: warning: mkdir XDG_RUNTIME_DIR parent %s: %v", parent, err)
	}
	rootfs = filepath.Clean(rootfs)
	for p := parent; len(p) > len(rootfs) && strings.HasPrefix(p, rootfs); p = filepath.Dir(p) {
		if fi, err := os.Stat(p); err == nil {
			if err := os.Chmod(p, fi.Mode().Perm()|0o011); err != nil {
				log.Printf("prepareRootfs: warning: chmod XDG_RUNTIME_DIR parent %s: %v", p, err)
			}
		}
	}
	if err := os.Mkdir(leaf, 0o700); err != nil && !os.IsExist(err) {
		log.Printf("prepareRootfs: warning: mkdir XDG_RUNTIME_DIR %s: %v", leaf, err)
	}
}

// chownLabelKey lets a container ask the daemon to chown host paths (bind-mount
// sources) to a fixed owner immediately before it starts — on every start,
// including restarts. It exists because a shared bind-mounted directory can be
// re-owned by a sibling container running under a different uid, breaking a
// process in this container that requires a specific owner. Canonical case:
// Wolf's pulseaudio runs as root and refuses to start unless its XDG_RUNTIME_DIR
// is root-owned, but the app containers Wolf launches run as uid 1000 and
// chown that shared dir — so after any app session a Wolf restart used to die
// until the dir was re-chowned by hand. Value: comma-separated "path[:uid[:gid]]"
// entries; owner defaults to 0:0 (root).
const chownLabelKey = "dld.chown"

// applyChownLabels honors chownLabelKey for the container, before it starts.
// Best-effort: a missing path or chown failure is logged, never fatal — the
// container should still start (and may simply work without the fix-up).
func (m *Manager) applyChownLabels(id string) {
	rec := m.store.GetContainer(id)
	if rec == nil {
		return
	}
	spec := strings.TrimSpace(rec.Labels[chownLabelKey])
	if spec == "" {
		return
	}
	for _, entry := range strings.Split(spec, ",") {
		if entry = strings.TrimSpace(entry); entry == "" {
			continue
		}
		path, uid, gid := parseChownEntry(entry)
		if path == "" {
			continue
		}
		if err := os.Chown(path, uid, gid); err != nil {
			log.Printf("applyChownLabels: chown %s -> %d:%d for %s: %v", path, uid, gid, shortID(id), err)
		}
	}
}

// parseChownEntry parses a "path[:uid[:gid]]" chown spec. Owner defaults to root
// (0:0); "path:uid" sets gid=uid, matching `chown uid`'s convention.
func parseChownEntry(entry string) (path string, uid, gid int) {
	parts := strings.Split(entry, ":")
	path = strings.TrimSpace(parts[0])
	if len(parts) >= 2 {
		if v, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil {
			uid, gid = v, v
		}
	}
	if len(parts) >= 3 {
		if v, err := strconv.Atoi(strings.TrimSpace(parts[2])); err == nil {
			gid = v
		}
	}
	return path, uid, gid
}

// StartContainer starts a stopped container.
// For Proxmox CTs (VMID > 0), uses pct start; otherwise lxc-start.
func (m *Manager) StartContainer(id string) error {
	state, _ := m.State(id)
	if state == "running" {
		return nil
	}

	rec := m.store.GetContainer(id)
	if rec != nil && rec.IPAddress != "" {
		if err := EnsureBridge(); err != nil {
			return fmt.Errorf("manager: bridge: %w", err)
		}
		// Seed this container's /etc/hosts before it starts so its first
		// outbound dial resolves sibling names — LXC2Docker has no
		// embedded DNS, so without this a name-addressed peer would be
		// unreachable until the periodic reconcile catches up.
		if err := m.writeContainerHosts(rec, m.store.ListContainers()); err != nil {
			log.Printf("container DNS: seed /etc/hosts for %s: %v", shortID(id), err)
		}
	}

	var err error
	if rec != nil && rec.VMID > 0 {
		err = m.startPVEContainer(id, rec.VMID)
	} else {
		err = m.startLXCContainer(id)
	}
	if err == nil {
		// Peers learn this container's (possibly reassigned) IP.
		m.syncHosts()
	}
	return err
}

func (m *Manager) startPVEContainer(id string, vmid int) error {
	m.applyChownLabels(id) // fix shared bind-mount ownership before init runs
	log.Printf("StartContainer[PVE]: pct start %d (%s)", vmid, id[:12])
	out, err := exec.Command("pct", "start", fmt.Sprintf("%d", vmid)).CombinedOutput()
	if err != nil {
		// pct start launches the container, then queries its init PID. When the
		// container's command exits instantly (`echo`, `true`), that query fails
		// ("Failed to receive ... get_init_pid" / "not running?") and pct exits
		// non-zero — even though the container DID start and run. That's a normal
		// fast-exit container, not a start failure. (A genuine launch failure —
		// bad config/rootfs — errors earlier with a different message.)
		so := string(out)
		if strings.Contains(so, "get_init_pid") || strings.Contains(so, "not running?") {
			// pct couldn't read the init PID. Usually a genuine fast-exit
			// (echo/true), but privileged/nested CTs (e.g. Wolf) can hit this
			// while actually running. maybeLeaseLANDHCP polls for the PID, so it
			// leases when the CT is really up and no-ops otherwise.
			log.Printf("StartContainer[PVE]: VMID %d (%s) started and exited immediately", vmid, id[:12])
			m.maybeLeaseLANDHCP(id)
			return nil
		}
		// Dump config for debugging a real failure.
		if cfgData, readErr := os.ReadFile(pveConfigPath(vmid)); readErr == nil {
			log.Printf("StartContainer[PVE]: FAILED config for VMID %d:\n%s", vmid, cfgData)
		}
		return fmt.Errorf("manager: pct start %d: %s: %w", vmid, out, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	// Ramp the poll: a warm reused CT (linked clone / adopted rootfs) is often
	// RUNNING within tens of ms, so a flat 200ms tick would add up to ~200ms of
	// dead wait to every fast launch. Start at 20ms and back off to 200ms so a
	// quick start is detected promptly without busy-spinning on a slow one.
	interval := 20 * time.Millisecond
	for time.Now().Before(deadline) {
		state, _ := m.State(id)
		if state == "running" {
			log.Printf("StartContainer[PVE]: VMID %d (%s) is running", vmid, id[:12])
			m.maybeLeaseLANDHCP(id)
			return nil
		}
		if state == "exited" {
			// `pct start` returned 0 (the start succeeded) but the container's
			// command already exited — a short-lived `echo`/`true`. That's a
			// normal fast-exit container, not a start failure.
			log.Printf("StartContainer[PVE]: VMID %d (%s) started and exited quickly", vmid, id[:12])
			return nil
		}
		time.Sleep(interval)
		if interval < 200*time.Millisecond {
			interval *= 2
		}
	}
	return fmt.Errorf("manager: VMID %d did not reach RUNNING within 30s", vmid)
}

// maybeLeaseLANDHCP gives the container's LAN NIC a DHCP lease when it requested
// one (gow.lan.ip=dhcp). PVE silently ignores custom LXC hooks, so the daemon
// drives the DHCP client itself once the container's netns is up. Best-effort:
// failures are logged, never fatal to the start.
func (m *Manager) maybeLeaseLANDHCP(id string) {
	rec := m.store.GetContainer(id)
	if rec == nil || !strings.EqualFold(rec.Labels["gow.lan.ip"], "dhcp") {
		return
	}
	// pct doesn't always expose the init PID immediately for privileged/nested
	// CTs; poll briefly so we lease once the netns is actually up.
	var pid int
	for i := 0; i < 30; i++ {
		if pid = m.InitPID(id); pid > 0 {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if pid <= 0 {
		log.Printf("LAN DHCP: no init PID for %s, skipping lease", id[:12])
		return
	}
	if err := leaseLANDHCP(pid); err != nil {
		log.Printf("LAN DHCP: %s: %v", id[:12], err)
		return
	}
	log.Printf("LAN DHCP: leased LAN address for %s", id[:12])
}

func (m *Manager) startLXCContainer(id string) error {
	m.applyChownLabels(id) // fix shared bind-mount ownership before init runs
	log.Printf("StartContainer[LXC]: starting %s", id)
	// -d (daemonize): modern lxc-start runs in the FOREGROUND by default, which
	// would block here until the container exits. Daemonize so the call returns
	// and the wait-for-RUNNING loop below can do its job.
	out, err := exec.Command("lxc-start", "-d", "-n", id, "--lxcpath", m.lxcPath,
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
			if err := m.ensureBridgeAttachment(id); err != nil {
				return err
			}
			log.Printf("StartContainer[LXC]: %s is running", id)
			return nil
		}
		if state == "exited" {
			// lxc-start succeeded but the command already exited (short-lived
			// `echo`/`true`). A normal fast-exit container — no bridge to
			// attach since it's already gone.
			log.Printf("StartContainer[LXC]: %s started and exited quickly", id)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("manager: container %s did not reach RUNNING within 30s", id)
}

func (m *Manager) ensureBridgeAttachment(id string) error {
	var link string
	var out []byte
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err = exec.Command("lxc-info", "-n", id, "--lxcpath", m.lxcPath).CombinedOutput()
		if err != nil {
			return fmt.Errorf("manager: inspect network link for %s: %s: %w", id, out, err)
		}
		link = lxcInfoLink(string(out))
		if link != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if link == "" {
		return fmt.Errorf("manager: inspect network link for %s: no Link in lxc-info output: %s", id, out)
	}

	out, err = exec.Command("ip", "-o", "link", "show", link).CombinedOutput()
	if err != nil {
		return fmt.Errorf("manager: inspect host link %s: %s: %w", link, out, err)
	}
	if strings.Contains(string(out), " master "+BridgeName+" ") {
		return nil
	}
	if out, err = exec.Command("ip", "link", "set", link, "master", BridgeName).CombinedOutput(); err != nil {
		return fmt.Errorf("manager: attach host link %s to %s: %s: %w", link, BridgeName, out, err)
	}
	if out, err = exec.Command("ip", "link", "set", link, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("manager: set host link %s up: %s: %w", link, out, err)
	}
	log.Printf("StartContainer[LXC]: attached host link %s to %s for %s", link, BridgeName, id[:12])
	return nil
}

func lxcInfoLink(out string) string {
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "Link" {
			continue
		}
		if fields := strings.Fields(value); len(fields) > 0 {
			return fields[0]
		}
	}
	return ""
}

// StopContainer stops a running container gracefully, waiting up to timeout.
// For Proxmox CTs uses pct shutdown; otherwise lxc-stop.
func (m *Manager) StopContainer(id string, timeout time.Duration) error {
	return m.StopContainerWithSignal(id, timeout, "")
}

// StopAllContainers stops every running container tracked by the daemon.
// LXC moves monitors and payloads into their own top-level cgroups, so systemd
// cannot clean them up by killing the daemon service cgroup alone.
func (m *Manager) StopAllContainers(timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	containers := m.store.ListContainers()
	var wg sync.WaitGroup
	errs := make(chan error, len(containers))
	stopping := 0

	for _, rec := range containers {
		if rec == nil {
			continue
		}
		state, err := m.State(rec.ID)
		if err != nil {
			log.Printf("shutdown: inspect %s (%s): %v", rec.Name, rec.ID[:12], err)
		}
		if state != "running" && state != "paused" {
			continue
		}

		stopping++
		wg.Add(1)
		go func(rec *store.ContainerRecord, state string) {
			defer wg.Done()
			log.Printf("shutdown: stopping container %s (%s)", rec.Name, rec.ID[:12])
			if state == "paused" {
				if err := m.UnpauseContainer(rec.ID); err != nil {
					log.Printf("shutdown: unpause %s (%s): %v", rec.Name, rec.ID[:12], err)
				}
			}
			if err := m.StopContainerWithSignal(rec.ID, timeout, rec.StopSignal); err != nil {
				errs <- fmt.Errorf("stop %s (%s): %w", rec.Name, rec.ID[:12], err)
			}
		}(rec, state)
	}

	wg.Wait()
	close(errs)

	var joined error
	for err := range errs {
		joined = errors.Join(joined, err)
	}
	if stopping > 0 {
		log.Printf("shutdown: requested stop for %d container(s)", stopping)
	}
	return joined
}

func (m *Manager) StopContainerWithSignal(id string, timeout time.Duration, signal string) error {
	state, _ := m.State(id)
	if state != "running" {
		return nil
	}

	rec := m.store.GetContainer(id)
	if rec != nil && rec.VMID > 0 {
		vmid := fmt.Sprintf("%d", rec.VMID)
		// Docker semantics: deliver the container's stop signal (default
		// SIGTERM) to its init PID, wait up to the timeout, then hard-stop.
		// `pct shutdown` / lxc-stop send SIGPWR (the LXC halt signal), which
		// only systemd-style inits treat as a shutdown request; application
		// inits (supervisord, Wolf, ...) don't handle SIGPWR and PID 1 ignores
		// unhandled signals, so graceful shutdown never runs and we always fall
		// through to a hard kill. Sending the real signal to init ourselves
		// matches Docker and lets in-container handlers run.
		sig := signal
		if sig == "" {
			sig = "SIGTERM"
		}
		if err := m.KillContainer(id, sig); err != nil {
			log.Printf("manager: stop %s: signal %s to init failed: %v (forcing)", id, sig, err)
		}
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if st, _ := m.State(id); st != "running" {
				return nil
			}
			time.Sleep(200 * time.Millisecond)
		}
		// Grace period elapsed: hard stop.
		out, err := exec.Command("pct", "stop", vmid).CombinedOutput()
		if err != nil {
			return fmt.Errorf("manager: pct stop %d: %s: %w", rec.VMID, out, err)
		}
		return nil
	}

	// Raw LXC (non-PVE) container: same Docker semantics as the PVE branch.
	// Deliver the stop signal (default SIGTERM) to init, wait up to the
	// timeout, then force-kill. Plain `lxc-stop` sends SIGPWR (the LXC halt
	// signal), which application inits ignore, so it never stops them
	// gracefully. KillContainer already routes SIGKILL to `lxc-stop --kill`
	// and other signals to `kill -<sig> <initPID>`.
	sig := signal
	if sig == "" {
		sig = "SIGTERM"
	}
	if err := m.KillContainer(id, sig); err != nil {
		log.Printf("manager: stop %s: signal %s to init failed: %v (forcing)", id, sig, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s, _ := m.State(id); s != "running" {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	out, err := exec.Command("lxc-stop", "--kill", "-n", id, "--lxcpath", m.lxcPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("manager: force stop %s: %s: %w", id, out, err)
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
	pid := m.InitPID(id)
	if pid <= 0 {
		return fmt.Errorf("manager: kill %s: container not running (no init pid)", id)
	}
	killOut, err := exec.Command("kill", fmt.Sprintf("-%s", signal), fmt.Sprintf("%d", pid)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("manager: kill %s (pid %d, signal %s): %s: %w", id, pid, signal, killOut, err)
	}
	return nil
}

// InitPID returns the host PID of a container's init process, or 0 if the
// container is not running. It is name-scheme aware: Proxmox CTs (VMID > 0)
// are addressed by VMID on the default lxcpath, while legacy containers use
// the Docker ID under the daemon's lxcpath.
func (m *Manager) InitPID(id string) int {
	rec := m.store.GetContainer(id)
	var out []byte
	var err error
	if rec != nil && rec.VMID > 0 {
		out, err = exec.Command("lxc-info", "-n", fmt.Sprintf("%d", rec.VMID), "-pH").Output()
	} else {
		out, err = exec.Command("lxc-info", "-n", id, "--lxcpath", m.lxcPath, "-pH").Output()
	}
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	if pid <= 0 {
		return 0
	}
	return pid
}

// RemoveContainer destroys a container and removes it from the store.
// Routes to pct destroy (PVE CT), ZFS destroy (ephemeral PVE), or
// lxc-destroy (legacy) based on container type.
func (m *Manager) RemoveContainer(id string) error {
	rec := m.store.GetContainer(id)
	if rec == nil {
		return m.store.RemoveContainer(id)
	}
	if !m.containerExists(id) {
		log.Printf("RemoveContainer: %s missing from LXC; removing stale store entry", id)
		return m.store.RemoveContainer(id)
	}

	state, _ := m.State(id)
	if state == "running" {
		return fmt.Errorf("manager: cannot remove running container %s; stop it first", id)
	}

	// Drop the per-container state dir (the id-embedded /etc/hostname source).
	os.RemoveAll(filepath.Join(m.store.RootDir(), "containers", id))

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
		// dataset, then remove the LXC config directory.
		cloneDataset := fmt.Sprintf("%s/lxc-%s", m.pveStorage, id)
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
		if lxcDestroyMissing(out) {
			log.Printf("RemoveContainer: lxc-destroy reported %s missing; removing stale store entry", id)
			return m.store.RemoveContainer(id)
		}
		return fmt.Errorf("manager: destroy %s: %s: %w", id, out, err)
	}
	return m.store.RemoveContainer(id)
}

func lxcDestroyMissing(out []byte) bool {
	msg := strings.ToLower(string(out))
	return strings.Contains(msg, "container is not defined") || strings.Contains(msg, "is not defined")
}

// RemoveImage destroys the template container and removes the image record.
func (m *Manager) RemoveImage(ref string) error {
	rec := m.store.GetImage(ref)
	if rec == nil {
		return fmt.Errorf("manager: image %q not found", ref)
	}

	if rec.TemplateTarball != "" {
		// Tarball-backed image (current scheme): just remove the tarball. No
		// Proxmox template CT exists, so there is nothing to pct-destroy. Log
		// (don't fail) on a real removal error so a left-behind tarball is
		// visible — reapOrphanTarballs is the backstop that eventually clears
		// it once the store record is gone.
		if err := os.Remove(rec.TemplateTarball); err != nil && !os.IsNotExist(err) {
			log.Printf("RemoveImage: remove tarball %s: %v", rec.TemplateTarball, err)
		}
		// Drop the ZFS template dataset (CoW clone origin), if one was
		// materialized. Best-effort and non-recursive: `zfs destroy` fails
		// while clones (running containers) still depend on the @base
		// snapshot, leaving the cache in place until they're gone.
		if rec.TemplateDataset != "" {
			if out, err := exec.Command("zfs", "destroy", "-r", rec.TemplateDataset).CombinedOutput(); err != nil {
				log.Printf("RemoveImage: zfs destroy %s: %s: %v (continuing)", rec.TemplateDataset, strings.TrimSpace(string(out)), err)
			}
		}
		// Drop the linked-clone template CT (lvmthin fast path), if one was
		// materialized. Best-effort: on lvmthin the base volume can be removed
		// while linked clones still reference its blocks.
		if rec.TemplateVMID > 0 {
			if out, err := exec.Command("pct", "destroy", fmt.Sprintf("%d", rec.TemplateVMID), "--force").CombinedOutput(); err != nil {
				log.Printf("RemoveImage: pct destroy template %d: %s: %v (continuing)", rec.TemplateVMID, strings.TrimSpace(string(out)), err)
			}
		}
		return m.store.RemoveImage(ref)
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

// PauseContainer freezes the container's processes. Uses pct suspend for PVE
// CTs (which writes the freezer cgroup) and lxc-freeze for legacy containers.
func (m *Manager) PauseContainer(id string) error {
	if rec := m.store.GetContainer(id); rec != nil && rec.VMID > 0 {
		out, err := exec.Command("pct", "suspend", fmt.Sprintf("%d", rec.VMID)).CombinedOutput()
		if err != nil {
			return fmt.Errorf("manager: pct suspend %d: %s: %w", rec.VMID, out, err)
		}
		return nil
	}
	out, err := exec.Command("lxc-freeze", "-n", id, "--lxcpath", m.lxcPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("manager: freeze %s: %s: %w", id, out, err)
	}
	return nil
}

// UnpauseContainer resumes a frozen container.
func (m *Manager) UnpauseContainer(id string) error {
	if rec := m.store.GetContainer(id); rec != nil && rec.VMID > 0 {
		out, err := exec.Command("pct", "resume", fmt.Sprintf("%d", rec.VMID)).CombinedOutput()
		if err != nil {
			return fmt.Errorf("manager: pct resume %d: %s: %w", rec.VMID, out, err)
		}
		return nil
	}
	out, err := exec.Command("lxc-unfreeze", "-n", id, "--lxcpath", m.lxcPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("manager: unfreeze %s: %s: %w", id, out, err)
	}
	return nil
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
	case "frozen":
		return "paused", nil
	case "stopped":
		return "exited", nil
	default:
		return lxcState, nil
	}
}

// Exec runs cmd inside the container. For PVE CTs uses pct exec;
// otherwise lxc-attach. Returns an *exec.Cmd not yet started.
func (m *Manager) Exec(id string, cmd []string, env []string) *exec.Cmd {
	return m.ExecAs(id, cmd, env, "")
}

// ExecAs is like Exec but honors a Docker-style user spec ("uid",
// "uid:gid", "user", or "user:group"). Passed to lxc-attach via -u/-g. PVE
// mode doesn't support arbitrary UID/GID via pct exec, so we wrap the
// command with su -c in that case.
func (m *Manager) ExecAs(id string, cmd, env []string, user string) *exec.Cmd {
	if rec := m.store.GetContainer(id); rec != nil && rec.VMID > 0 {
		args := []string{"exec", fmt.Sprintf("%d", rec.VMID), "--"}
		if user != "" {
			args = append(args, suWrap(cmd, user)...)
		} else {
			args = append(args, cmd...)
		}
		c := exec.Command("pct", args...)
		c.Env = env
		return c
	}
	args := []string{"-n", id, "--lxcpath", m.lxcPath}
	runArgv := cmd
	if user != "" {
		// lxc-attach -u/-g accept only NUMERIC ids. For a numeric spec, pass
		// them directly; for a user NAME, run the command through `su` inside
		// the container so the name resolves against the container's own passwd
		// database (same approach as the pct-exec branch above and buildRunArgv).
		// Handing a name to `lxc-attach -u` makes it abort with
		// "could not parse command line".
		if uid, gid, ok := numericUserSpec(user); ok {
			if uid != "" {
				args = append(args, "-u", uid)
			}
			if gid != "" {
				args = append(args, "-g", gid)
			}
		} else {
			runArgv = suWrap(cmd, user)
		}
	}
	args = append(args, "--")
	args = append(args, runArgv...)
	c := exec.Command("lxc-attach", args...)
	c.Env = env
	return c
}

// suWrap returns an argv that runs cmd as the given user via `su` inside the
// container. su resolves a user NAME against the container's own passwd
// database — unlike `lxc-attach -u`, which accepts only numeric ids. Shared by
// the pct-exec and lxc-attach exec paths so their user handling can't drift.
func suWrap(cmd []string, user string) []string {
	return []string{"su", "-s", "/bin/sh", "-c", shellJoin(cmd), userName(user)}
}

// numericUserSpec reports whether a "uid[:gid]" spec is purely numeric and, if
// so, returns its parts. Only numeric specs are valid for `lxc-attach -u/-g`;
// a user NAME must instead be resolved inside the container (see suWrap).
func numericUserSpec(s string) (uid, gid string, ok bool) {
	uid, gid, _ = strings.Cut(s, ":")
	if !isAllDigits(uid) {
		return "", "", false
	}
	if gid != "" && !isAllDigits(gid) {
		return "", "", false
	}
	return uid, gid, true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func userName(s string) string {
	u, _, _ := strings.Cut(s, ":")
	return u
}

func shellJoin(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("'" + strings.ReplaceAll(a, "'", `'\''`) + "'")
	}
	return b.String()
}

// LogPath returns the console log file path for a container.
func (m *Manager) LogPath(id string) string {
	return LogFilePath(m.lxcPath, id)
}

// LXCPath returns the container storage root.
func (m *Manager) LXCPath() string { return m.lxcPath }

// ImageRootfsPath resolves a template image ref to a filesystem rootfs path,
// if available. It returns an empty string when the image is not backed by a
// readable directory path. For PVE-backed templates this returns the dataset
// mountpoint path where the base rootfs is typically exposed.
func (m *Manager) ImageRootfsPath(ref string) string {
	rec := m.store.GetImage(ref)
	if rec == nil {
		return ""
	}
	if rec.TemplateName != "" {
		return filepath.Join(m.lxcPath, rec.TemplateName, "rootfs")
	}
	if m.UsePVE() && rec.TemplateVMID > 0 {
		return fmt.Sprintf("/%s/basevol-%d-disk-0", m.pveStorage, rec.TemplateVMID)
	}
	return ""
}

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

// ImageReady reports whether the store record still points at a usable clone
// source. This lets the API self-heal stale image metadata by triggering a
// fresh pull instead of attempting to clone a missing legacy template.
func (m *Manager) ImageReady(rec *store.ImageRecord) bool {
	if rec == nil {
		return false
	}
	// Tarball-backed image (current PVE scheme): ready iff the rootfs tarball
	// is on disk. Without this check ImageReady falls through to the legacy
	// VMID/template-dir probes — which a tarball image never satisfies — so
	// the create path treats an already-pulled image as missing and re-pulls
	// it under the raw request ref, producing a duplicate image record.
	if rec.TemplateTarball != "" {
		_, err := os.Stat(rec.TemplateTarball)
		return err == nil
	}
	if rec.TemplateVMID > 0 {
		if _, err := os.Stat(pveConfigPath(rec.TemplateVMID)); err == nil {
			return true
		}
	}
	if rec.TemplateName == "" {
		return false
	}
	configPath := filepath.Join(m.lxcPath, rec.TemplateName, "config")
	_, err := os.Stat(configPath)
	return err == nil
}

// --- helpers ---

var hostResolvConfPaths = []string{
	"/run/systemd/resolve/resolv.conf",
	"/etc/resolv.conf",
}

const defaultDNSOptions = "timeout:2 attempts:2 rotate"

var fallbackNameservers = []string{"1.1.1.1", "8.8.8.8"}

func buildResolvConf(cfg ContainerConfig) string {
	var b strings.Builder
	if len(cfg.DNS) == 0 {
		b.WriteString(defaultResolvConf())
	} else {
		for _, d := range cfg.DNS {
			d = strings.TrimSpace(d)
			if d != "" && !strings.ContainsAny(d, "\r\n") {
				b.WriteString("nameserver ")
				b.WriteString(d)
				b.WriteByte('\n')
			}
		}
	}
	if len(cfg.DNSSearch) > 0 {
		b.WriteString("search")
		for _, s := range cfg.DNSSearch {
			s = strings.TrimSpace(s)
			if s != "" && !strings.ContainsAny(s, "\r\n") {
				b.WriteByte(' ')
				b.WriteString(s)
			}
		}
		b.WriteByte('\n')
	}
	for _, o := range cfg.DNSOptions {
		o = strings.TrimSpace(o)
		if o != "" && !strings.ContainsAny(o, "\r\n") {
			b.WriteString("options ")
			b.WriteString(o)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func defaultResolvConf() string {
	servers := hostNameservers()
	if len(servers) == 0 {
		servers = fallbackNameservers
	}
	var b strings.Builder
	for _, server := range servers {
		b.WriteString("nameserver ")
		b.WriteString(server)
		b.WriteByte('\n')
	}
	b.WriteString("options ")
	b.WriteString(defaultDNSOptions)
	b.WriteByte('\n')
	return b.String()
}

func hostNameservers() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, path := range hostResolvConfPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, server := range parseNameservers(string(data)) {
			if seen[server] {
				continue
			}
			seen[server] = true
			out = append(out, server)
		}
		if len(out) > 0 {
			return out
		}
	}
	return out
}

func parseNameservers(data string) []string {
	out := []string{}
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		server := strings.TrimSpace(fields[1])
		if server == "" || strings.ContainsAny(server, "\r\n") || isLoopbackNameserver(server) {
			continue
		}
		out = append(out, server)
	}
	return out
}

func isLoopbackNameserver(server string) bool {
	return server == "::1" || server == "0:0:0:0:0:0:0:1" ||
		strings.HasPrefix(server, "127.") ||
		strings.EqualFold(server, "localhost")
}

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

// pveRootfsPath returns the rootfs path for a Proxmox CT on ZFS storage.
// For ZFS pool "large" and VMID 260: /large/subvol-260-disk-0
func (m *Manager) pveRootfsPath(vmid int) string {
	// Proxmox ZFS storage mounts datasets at /<pool>/subvol-<vmid>-disk-0.
	// Determine the ZFS mountpoint by checking pvesm.
	return fmt.Sprintf("/%s/subvol-%d-disk-0", m.pveStorage, vmid)
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

func (m *Manager) waitRunning(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if state, _ := m.State(name); state == "running" {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("container %s did not reach RUNNING within %s", name, timeout)
}

func (m *Manager) runInContainer(name, shellCmd string) error {
	out, err := exec.Command(
		"lxc-attach", "-n", name, "--lxcpath", m.lxcPath,
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
