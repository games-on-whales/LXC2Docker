package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// TestTranslateSiblingBindSource covers the Wolf black-screen fix: a parent
// container passes one of its own in-container paths as a sibling bind source,
// and the runtime must rewrite it to the real host path via the parent's
// mount mapping (and only then).
func TestTranslateSiblingBindSource(t *testing.T) {
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	h := &Handler{store: st}

	// A parent container (Wolf) bind-mounts a host dir at an in-container path.
	hostState := t.TempDir() // stands in for /mnt/<pool>/.plugins/wolf/state
	if err := st.AddContainer(&store.ContainerRecord{
		ID:   "wolf",
		Name: "wolf",
		Mounts: []store.MountSpec{
			{Type: "bind", Source: hostState, Destination: "/etc/wolf"},
		},
	}); err != nil {
		t.Fatalf("add container: %v", err)
	}

	// The app home exists on the host under the real (translated) path.
	appHome := filepath.Join(hostState, "sess1", "Wolf UI")
	if err := os.MkdirAll(appHome, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Wolf passes its OWN in-container path; translate it to the host path.
	if got := h.translateSiblingBindSource("/etc/wolf/sess1/Wolf UI"); got != appHome {
		t.Errorf("in-container source: got %q, want %q", got, appHome)
	}

	// A source that exists on the host is left untouched.
	if got := h.translateSiblingBindSource(hostState); got != hostState {
		t.Errorf("existing host path was rewritten: %q", got)
	}

	// A missing source whose translation also does not exist is left untouched.
	const missing = "/etc/wolf/sess1/Does Not Exist"
	if got := h.translateSiblingBindSource(missing); got != missing {
		t.Errorf("non-existent translation should not rewrite: got %q", got)
	}

	// A missing source matching no container destination is left untouched.
	const unrelated = "/no/such/mount/x"
	if got := h.translateSiblingBindSource(unrelated); got != unrelated {
		t.Errorf("unrelated path rewritten: %q", got)
	}
}

// TestTranslateSiblingBindSourceResolvesRuntimeDir covers Wolf's PulseAudio
// sibling: Wolf passes its own XDG_RUNTIME_DIR (/run/user/wolf) as the bind
// source, which has no host bind backing it but lives inside Wolf's rootfs.
// It must be rewritten to the host-side rootfs path so the sibling can mount it.
func TestTranslateSiblingBindSourceResolvesRuntimeDir(t *testing.T) {
	lxcPath := t.TempDir()
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	h := &Handler{store: st}

	// Stub the rootfs lookup so the test doesn't need a privileged LXC manager.
	// Mirrors RootfsPath's legacy layout: <lxcPath>/<id>/rootfs.
	orig := rootfsPathFor
	rootfsPathFor = func(_ *Handler, id string) string {
		return filepath.Join(lxcPath, id, "rootfs")
	}
	t.Cleanup(func() { rootfsPathFor = orig })

	if err := st.AddContainer(&store.ContainerRecord{ID: "wolf", Name: "wolf"}); err != nil {
		t.Fatalf("add container: %v", err)
	}

	// Wolf created its XDG_RUNTIME_DIR inside its rootfs at runtime.
	runtimeDir := filepath.Join(lxcPath, "wolf", "rootfs", "run", "user", "wolf")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatalf("mkdir runtime dir: %v", err)
	}

	if got := h.translateSiblingBindSource("/run/user/wolf"); got != runtimeDir {
		t.Errorf("runtime dir source: got %q, want %q", got, runtimeDir)
	}

	// A path that exists in no container's rootfs is left untouched.
	const missing = "/run/user/nobody"
	if got := h.translateSiblingBindSource(missing); got != missing {
		t.Errorf("unknown runtime dir rewritten: %q", got)
	}
}
