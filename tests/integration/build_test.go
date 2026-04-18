//go:build integration

package integration

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCanBuildDaemonBinary(t *testing.T) {
	t.Parallel()

	bin := filepath.Join(t.TempDir(), "docker-lxc-daemon")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/docker-lxc-daemon")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to build daemon: %v\n%s", err, string(out))
	}
}
