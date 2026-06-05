package lxc

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

// FreeBytes returns the bytes available to the daemon on the filesystem
// backing path, matching `df` semantics (statfs Bavail × block size). A
// non-existent path returns the error from statfs so callers can skip it.
func FreeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// fsKey identifies the filesystem a path lives on, so several paths on the
// same device are only checked (and alerted) once.
func fsKey(path string) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil
}

// DiskPressureEmitter publishes a disk-pressure notification (wired to the
// Docker event stream by the API layer). inPressure is true when free space
// has dropped below the threshold and false on the recovery edge.
type DiskPressureEmitter func(path string, freeBytes, thresholdBytes uint64, inPressure bool)

// SetMinFreeBytes configures the low-space threshold used by the create
// pre-flight and the disk-pressure watcher. Zero disables both.
func (m *Manager) SetMinFreeBytes(b uint64) { m.minFreeBytes = b }

// watchedPaths returns one representative path per distinct filesystem the
// daemon writes to: its state dir, the LXC path, the PVE storage mountpoint
// (when it has one), and every running container's bind-mount sources. These
// are exactly the filesystems a runaway write (e.g. a game download into a
// bind-mounted home) can fill out from under the host.
func (m *Manager) watchedPaths() []string {
	seen := map[uint64]bool{}
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		key, err := fsKey(p)
		if err != nil || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, p)
	}
	add(m.store.RootDir())
	add(m.lxcPath)
	if m.UsePVE() && m.pveStorageIsZFS() {
		add("/" + m.pveStorage)
	}
	for _, rec := range m.store.ListContainers() {
		if state, _ := m.State(rec.ID); state != "running" {
			continue
		}
		for _, mnt := range rec.Mounts {
			if mnt.Type == "tmpfs" || mnt.ReadOnly {
				continue
			}
			add(mnt.Source)
		}
	}
	sort.Strings(out)
	return out
}

// StartDiskPressureWatcher periodically checks the free space on every
// filesystem the daemon writes to and alerts (log + event) when one crosses
// below the configured threshold — turning a silent host-filling write into a
// visible warning. Alerts are edge-triggered per filesystem so a sustained
// low-space condition doesn't spam. A zero threshold disables the watcher.
func (m *Manager) StartDiskPressureWatcher(ctx context.Context, emit DiskPressureEmitter) {
	if m.minFreeBytes == 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		inPressure := map[string]bool{}
		check := func() {
			for _, p := range m.watchedPaths() {
				free, err := FreeBytes(p)
				if err != nil {
					continue
				}
				low := free < m.minFreeBytes
				switch {
				case low && !inPressure[p]:
					inPressure[p] = true
					log.Printf("disk-pressure: %s has %s free, below %s threshold",
						p, humanBytes(free), humanBytes(m.minFreeBytes))
					if emit != nil {
						emit(p, free, m.minFreeBytes, true)
					}
				case !low && inPressure[p]:
					delete(inPressure, p)
					log.Printf("disk-pressure: %s recovered (%s free)", p, humanBytes(free))
					if emit != nil {
						emit(p, free, m.minFreeBytes, false)
					}
				}
			}
		}
		check() // run once at startup rather than waiting a full interval
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check()
			}
		}
	}()
}

// checkCreateDiskPressure rejects a container create when one of its writable
// bind sources is already below the low-space threshold, so a launch onto an
// almost-full filesystem fails loudly instead of wedging the host. A zero
// threshold disables the check.
func (m *Manager) checkCreateDiskPressure(cfg ContainerConfig) error {
	if m.minFreeBytes == 0 {
		return nil
	}
	for _, mnt := range cfg.Mounts {
		if mnt.ReadOnly {
			continue
		}
		free, err := FreeBytes(mnt.Source)
		if err != nil {
			continue // source may be created on demand; don't block on stat errors
		}
		if free < m.minFreeBytes {
			return fmt.Errorf("manager: refusing create: bind source %s has only %s free (below %s threshold)",
				mnt.Source, humanBytes(free), humanBytes(m.minFreeBytes))
		}
	}
	return nil
}

// humanBytes formats a byte count as a compact human-readable string (GiB/MiB).
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
