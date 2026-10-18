package lxc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestColdTemplatePreservesRequestedVMIDAndReleasesForFallback(t *testing.T) {
	fakeCluster(t)
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef"
	if err := st.AddContainer(&store.ContainerRecord{ID: id, Name: "wolf"}); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	logPath := filepath.Join(bin, "calls")
	t.Setenv("PCT_TEST_LOG", logPath)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	// Fail both pct creates deliberately: template creation must use 101,
	// then the slow-path retry must still be able to allocate the pinned 100.
	if err := os.WriteFile(filepath.Join(bin, "pct"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$PCT_TEST_LOG\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := &Manager{store: st, pveStorage: "storage"}
	img := &store.ImageRecord{ID: "test-image", Ref: "test:latest", TemplateTarball: "/missing/test.tar"}
	err = mgr.createPVELinkedCloneFromTarball(id, img, ContainerConfig{RequestedVMID: 100})
	if err == nil || !strings.Contains(err.Error(), "pct create 100") {
		t.Fatalf("expected fallback pct create error for 100, got %v", err)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "create 101 ") || !strings.HasPrefix(lines[1], "create 100 ") {
		t.Fatalf("template stole requested ID or fallback failed: %s", calls)
	}
	if len(vmidReserved) != 0 {
		t.Fatalf("failed provisioning leaked reservations: %v", vmidReserved)
	}
}
