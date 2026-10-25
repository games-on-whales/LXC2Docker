package lxc

import (
	"bufio"
	"context"
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// StartExitMonitor records the exit code of a container whose init exits on
// its own (`docker run alpine sh -c 'exit 3'` must report 3). The init runs as
// the container's PID 1 with no wrapper, so the daemon never waits on it, and
// the PVE unit's `lxc-start -F` does not pass the status on to systemd. LXC's
// monitor channel does: lxc-monitor prints "'<name>' exited with status [N]"
// just before the container moves to STOPPING, ahead of the STOPPED state
// that ends a /containers/{id}/wait.
//
// lxc-monitor reports WEXITSTATUS of the init's wait status, so an init killed
// by a signal (an OOM kill, a host kill -9) reads as 0, as every exit did
// before. Stops and kills through the API record their own code and are
// skipped here.
func (m *Manager) StartExitMonitor(ctx context.Context) {
	paths := []string{m.lxcPath}
	if m.UsePVE() && m.lxcPath != pveLXCPath {
		paths = append(paths, pveLXCPath)
	}
	for _, p := range paths {
		go m.runExitMonitor(ctx, p)
	}
}

func (m *Manager) runExitMonitor(ctx context.Context, lxcPath string) {
	backoff := time.Second
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, "lxc-monitor", "--lxcpath", lxcPath, "-n", ".*")
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			log.Printf("exit-monitor %s: %v (container exit codes will read 0)", lxcPath, err)
		} else {
			backoff = time.Second
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				if name, code, ok := parseExitLine(sc.Text()); ok {
					m.recordExit(name, code)
				}
			}
			cmd.Wait()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

var exitLineRE = regexp.MustCompile(`^'(.+)' exited with status \[(-?\d+)\]$`)

// parseExitLine extracts the container name and exit code from an lxc-monitor
// "'<name>' exited with status [N]" line.
func parseExitLine(line string) (string, int, bool) {
	sub := exitLineRE.FindStringSubmatch(line)
	if sub == nil {
		return "", 0, false
	}
	code, err := strconv.Atoi(sub[2])
	if err != nil {
		return "", 0, false
	}
	return sub[1], code, true
}

// recordExit stores the exit code of the container LXC knows as name: a VMID
// for a Proxmox CT, the Docker ID for a legacy container. An exit the user
// asked for (stop/kill, which set StoppedByUser and record the signal's code
// first) keeps the code the API recorded.
func (m *Manager) recordExit(name string, code int) {
	rec := m.store.GetContainer(name)
	if rec == nil {
		if vmid, err := strconv.Atoi(name); err == nil && vmid > 0 {
			for _, c := range m.store.ListContainers() {
				if c.VMID == vmid {
					rec = c
					break
				}
			}
		}
	}
	if rec == nil {
		return
	}
	now := time.Now()
	if _, err := m.store.UpdateContainer(rec.ID, func(cur *store.ContainerRecord) {
		if cur.StoppedByUser {
			return
		}
		cur.ExitCode = code
		cur.FinishedAt = &now
	}); err != nil {
		log.Printf("exit-monitor: record exit %d for %s: %v", code, rec.ID[:12], err)
	}
}
