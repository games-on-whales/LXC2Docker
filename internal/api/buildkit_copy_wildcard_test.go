package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/buildkit/solver/pb"
)

// A COPY with a wildcard source (e.g. `COPY --from=builder /lib/*.so /dest/`)
// leaves the glob in the action's Src with AllowWildcard set; the executor must
// expand it and copy each match into the destination directory. Without that the
// literal `*` is stat'd and the build fails with "no such file or directory".
func TestApplyFileAction_CopyWildcard(t *testing.T) {
	srcRoot := t.TempDir()
	subdir := filepath.Join(srcRoot, "plugins")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"libone.so", "libtwo.so"} {
		if err := os.WriteFile(filepath.Join(subdir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	work := t.TempDir()
	e := &llbExecutor{}
	act := &pb.FileAction{
		SecondaryInput: 0, // copy from state[0], the secondary input root
		Action: &pb.FileAction_Copy{Copy: &pb.FileActionCopy{
			Src:           "/plugins/*",
			Dest:          "/dest/",
			AllowWildcard: true,
		}},
	}

	if err := e.applyFileAction(work, []string{srcRoot}, act); err != nil {
		t.Fatalf("applyFileAction: %v", err)
	}

	for _, name := range []string{"libone.so", "libtwo.so"} {
		if _, err := os.Stat(filepath.Join(work, "dest", name)); err != nil {
			t.Errorf("expected %s copied into dest: %v", name, err)
		}
	}
}

// A wildcard that matches nothing errors only when AllowEmptyWildcard is unset.
func TestApplyFileAction_CopyWildcardNoMatch(t *testing.T) {
	srcRoot := t.TempDir()
	work := t.TempDir()
	e := &llbExecutor{}
	act := &pb.FileAction{
		SecondaryInput: 0,
		Action: &pb.FileAction_Copy{Copy: &pb.FileActionCopy{
			Src:           "/nope/*",
			Dest:          "/dest/",
			AllowWildcard: true,
		}},
	}
	if err := e.applyFileAction(work, []string{srcRoot}, act); err == nil {
		t.Error("expected error for wildcard matching no files without AllowEmptyWildcard")
	}
}

// The Dockerfile frontend always sets AllowEmptyWildcard for COPY, so a wildcard
// matching nothing must be a no-op (the file `COPY /usr/local/lib/libfoo*` case
// where libfoo lives elsewhere) rather than failing the build.
func TestApplyFileAction_CopyWildcardEmptyAllowed(t *testing.T) {
	srcRoot := t.TempDir()
	work := t.TempDir()
	e := &llbExecutor{}
	act := &pb.FileAction{
		SecondaryInput: 0,
		Action: &pb.FileAction_Copy{Copy: &pb.FileActionCopy{
			Src:                "/nope/*",
			Dest:               "/dest/",
			AllowWildcard:      true,
			AllowEmptyWildcard: true,
		}},
	}
	if err := e.applyFileAction(work, []string{srcRoot}, act); err != nil {
		t.Errorf("AllowEmptyWildcard: a no-match must be a no-op, got %v", err)
	}
}
