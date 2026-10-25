package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// shortTempDir is a temp dir whose name does not embed the test name:
// t.TempDir() does, which pushes socket paths toward the 108-byte sun_path
// limit, where binds and connects start failing.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dld")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// listenUnix serves path for the rest of the test.
func listenUnix(t *testing.T, path string) *net.UnixListener {
	t.Helper()
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func readlink(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return target
}

func TestLinkCompatSocketCreatesMissingLink(t *testing.T) {
	dir := shortTempDir(t)
	compat, ours := filepath.Join(dir, "docker.sock"), filepath.Join(dir, "dld.sock")

	if !linkCompatSocket(compat, ours) {
		t.Fatal("linkCompatSocket did not report creating the link")
	}
	if got := readlink(t, compat); got != ours {
		t.Fatalf("compat link -> %q, want %q", got, ours)
	}
}

// The bug this pins: a test instance started with its own --socket deleted a
// host's real dockerd socket and left a link that dangled once it exited.
func TestLinkCompatSocketLeavesALiveForeignSocket(t *testing.T) {
	dir := shortTempDir(t)
	compat, ours := filepath.Join(dir, "docker.sock"), filepath.Join(dir, "dld.sock")
	listenUnix(t, compat) // a real dockerd

	if linkCompatSocket(compat, ours) {
		t.Fatal("linkCompatSocket took over a socket another daemon is serving")
	}
	if fi, err := os.Lstat(compat); err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("live foreign socket was replaced: %v %v", fi, err)
	}
}

// Another docker-lxc-daemon instance's link to its live socket stays put too.
func TestLinkCompatSocketLeavesALinkToAnotherLiveDaemon(t *testing.T) {
	dir := shortTempDir(t)
	compat, ours, theirs := filepath.Join(dir, "docker.sock"), filepath.Join(dir, "dld.sock"), filepath.Join(dir, "prod.sock")
	listenUnix(t, theirs)
	if err := os.Symlink(theirs, compat); err != nil {
		t.Fatal(err)
	}

	if linkCompatSocket(compat, ours) {
		t.Fatal("linkCompatSocket replaced a link to another live daemon")
	}
	if got := readlink(t, compat); got != theirs {
		t.Fatalf("compat link -> %q, want %q", got, theirs)
	}
}

func TestLinkCompatSocketReplacesADanglingLink(t *testing.T) {
	dir := shortTempDir(t)
	compat, ours := filepath.Join(dir, "docker.sock"), filepath.Join(dir, "dld.sock")
	if err := os.Symlink(filepath.Join(dir, "gone.sock"), compat); err != nil {
		t.Fatal(err)
	}

	if !linkCompatSocket(compat, ours) {
		t.Fatal("linkCompatSocket did not take over a dangling link")
	}
	if got := readlink(t, compat); got != ours {
		t.Fatalf("compat link -> %q, want %q", got, ours)
	}
}

func TestLinkCompatSocketReplacesADeadSocket(t *testing.T) {
	dir := shortTempDir(t)
	compat, ours := filepath.Join(dir, "docker.sock"), filepath.Join(dir, "dld.sock")
	l := listenUnix(t, compat)
	l.SetUnlinkOnClose(false)
	l.Close() // the file stays, nobody answers

	if !linkCompatSocket(compat, ours) {
		t.Fatal("linkCompatSocket did not take over a dead socket")
	}
	if got := readlink(t, compat); got != ours {
		t.Fatalf("compat link -> %q, want %q", got, ours)
	}
}

func TestLinkCompatSocketLeavesARegularFile(t *testing.T) {
	dir := shortTempDir(t)
	compat, ours := filepath.Join(dir, "docker.sock"), filepath.Join(dir, "dld.sock")
	if err := os.WriteFile(compat, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if linkCompatSocket(compat, ours) {
		t.Fatal("linkCompatSocket replaced a regular file")
	}
	if _, err := os.Stat(compat); err != nil {
		t.Fatalf("regular file removed: %v", err)
	}
}

// A restart finds its own link from the previous run and keeps ownership, so
// shutdown still removes it.
func TestLinkCompatSocketKeepsItsOwnLink(t *testing.T) {
	dir := shortTempDir(t)
	compat, ours := filepath.Join(dir, "docker.sock"), filepath.Join(dir, "dld.sock")
	if err := os.Symlink(ours, compat); err != nil {
		t.Fatal(err)
	}

	if !linkCompatSocket(compat, ours) {
		t.Fatal("linkCompatSocket did not claim its own link")
	}
}

func TestLinkCompatSocketIsANoOpWhenServingThePathDirectly(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "docker.sock")
	if linkCompatSocket(path, path) {
		t.Fatal("linkCompatSocket claimed a link when --socket is the compat path")
	}
}

func TestUnlinkCompatSocketRemovesOnlyItsOwnLink(t *testing.T) {
	dir := shortTempDir(t)
	compat, ours, theirs := filepath.Join(dir, "docker.sock"), filepath.Join(dir, "dld.sock"), filepath.Join(dir, "prod.sock")

	if err := os.Symlink(theirs, compat); err != nil {
		t.Fatal(err)
	}
	unlinkCompatSocket(compat, ours)
	if got := readlink(t, compat); got != theirs {
		t.Fatalf("unlink removed someone else's link (now %q)", got)
	}

	os.Remove(compat)
	if err := os.Symlink(ours, compat); err != nil {
		t.Fatal(err)
	}
	unlinkCompatSocket(compat, ours)
	if _, err := os.Lstat(compat); !os.IsNotExist(err) {
		t.Fatalf("own link not removed on shutdown: %v", err)
	}
}
