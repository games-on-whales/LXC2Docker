package lxc

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func transientBuildID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// RunInRootfs runs argv inside a transient, networked LXC container whose rootfs
// is rootfsDir, returning the command's combined output. The container provides
// a real /proc, reliable exec and bridge networking (so apt/gcc work) — the
// BuildKit executor uses it to run RUN steps without a chroot, matching the
// classic builder's container-based execution.
//
// The builder id is "build-"-prefixed so the GC leaves it alone, and no store
// record is created, so listings/events never see the transient container.
// rootfsDir is mutated in place and left intact; only the transient config/log
// directory is removed.
func (m *Manager) RunInRootfs(rootfsDir string, argv, env []string) ([]byte, error) {
	if err := EnsureBridge(); err != nil {
		return nil, fmt.Errorf("bridge: %w", err)
	}

	id := "build-" + transientBuildID()
	containerDir := filepath.Join(m.lxcPath, id)
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		return nil, err
	}
	defer os.RemoveAll(containerDir)

	ip, err := m.store.AllocateIP()
	if err != nil {
		return nil, fmt.Errorf("allocate build IP: %w", err)
	}
	defer m.store.FreeIP(ip)

	cfg := ContainerConfig{
		Env: env,
		// Idle init so the container stays up for lxc-attach; the build command
		// runs via ExecAs, not as PID 1.
		Cmd:     []string{"sleep", "infinity"},
		LogFile: LogFilePath(m.lxcPath, id),
	}
	if err := os.MkdirAll(filepath.Dir(cfg.LogFile), 0o755); err != nil {
		return nil, err
	}

	// Minimal base config; rewriteConfig appends networking, apparmor=unconfined,
	// proc/sys mounts and the lxc.init.cmd from cfg.Cmd.
	base := fmt.Sprintf("lxc.include = /usr/share/lxc/config/common.conf\nlxc.arch = linux64\nlxc.rootfs.path = dir:%s\nlxc.uts.name = %s\n",
		rootfsDir, id)
	configPath := filepath.Join(containerDir, "config")
	if err := os.WriteFile(configPath, []byte(base), 0o644); err != nil {
		return nil, err
	}
	if err := rewriteConfig(configPath, &cfg, ip, id, true); err != nil {
		return nil, fmt.Errorf("write builder config: %w", err)
	}
	m.prepareRootfs(rootfsDir, cfg)

	if err := m.startLXCContainer(id); err != nil {
		return nil, fmt.Errorf("start builder container: %w", err)
	}
	defer func() { _ = m.StopContainer(id, 5*time.Second) }()

	// User is baked into argv by the caller (su wrapper), matching the classic
	// builder; lxc-attach runs as root.
	return m.ExecAs(id, argv, env, "").CombinedOutput()
}
