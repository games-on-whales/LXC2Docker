package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/buildkit/solver/pb"
	"github.com/opencontainers/go-digest"
)

func newTestExecutor(t *testing.T, ctxDir string) *llbExecutor {
	t.Helper()
	return &llbExecutor{
		ctxDir:    ctxDir,
		scratch:   t.TempDir(),
		emit:      func(string) {},
		byDigest:  map[digest.Digest]*pb.Op{},
		opOutputs: map[digest.Digest]map[int64]string{},
		images:    map[string]string{},
	}
}

// TestExecFileActionChaining exercises the LLB File op state model: a scratch
// Mkdir whose output feeds a Copy from the build context, mirroring how
// dockerfile2llb lowers `WORKDIR /app` + `COPY hello.txt .`.
func TestExecFileActionChaining(t *testing.T) {
	ctxDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ctxDir, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := newTestExecutor(t, ctxDir)

	inputDirs := []string{ctxDir} // state index 0 == build context

	file := &pb.FileOp{Actions: []*pb.FileAction{
		// action 0: mkdir -p /app on an empty scratch base; intermediate only.
		{
			Input:          int64(pb.Empty),
			SecondaryInput: int64(pb.Empty),
			Output:         int64(pb.SkipOutput),
			Action:         &pb.FileAction_Mkdir{Mkdir: &pb.FileActionMkDir{Path: "/app", Mode: 0o755, MakeParents: true}},
		},
		// action 1: copy context hello.txt into /app, basing on action 0's
		// output (state index len(inputs)+0 == 1), exposed as op output 0.
		{
			Input:          1,
			SecondaryInput: 0,
			Output:         0,
			Action:         &pb.FileAction_Copy{Copy: &pb.FileActionCopy{Src: "/hello.txt", Dest: "/app/hello.txt"}},
		},
	}}

	outs, err := e.execFile(file, inputDirs)
	if err != nil {
		t.Fatalf("execFile: %v", err)
	}
	resultDir, ok := outs[0]
	if !ok {
		t.Fatal("no output 0 from file op")
	}
	got, err := os.ReadFile(filepath.Join(resultDir, "app", "hello.txt"))
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if string(got) != "hi" {
		t.Fatalf("copied content = %q, want hi", got)
	}
}

// TestStageSecretWritesFromSession verifies RUN --mount=type=secret writes the
// session-provided value into the root snapshot at the mount destination.
func TestStageSecretWritesFromSession(t *testing.T) {
	e := newTestExecutor(t, "")
	e.ctx = context.Background()
	e.getSecret = func(ctx context.Context, id string) ([]byte, error) {
		if id != "mytoken" {
			return nil, fmt.Errorf("unknown secret %q", id)
		}
		return []byte("s3cr3t"), nil
	}
	rootDir := t.TempDir()
	m := &pb.Mount{
		MountType: pb.MountType_SECRET,
		Dest:      "/run/secrets/token",
		SecretOpt: &pb.SecretOpt{ID: "mytoken", Mode: 0o400},
	}
	path, err := e.stageSecret(rootDir, m)
	if err != nil {
		t.Fatalf("stageSecret: %v", err)
	}
	if path == "" {
		t.Fatal("expected a secret file path for post-run cleanup")
	}
	got, err := os.ReadFile(filepath.Join(rootDir, "run/secrets/token"))
	if err != nil || string(got) != "s3cr3t" {
		t.Fatalf("secret file = %q err=%v, want s3cr3t", got, err)
	}
}

// TestStageSecretOptionalMissingSkips verifies an optional secret that the
// session can't supply is skipped rather than failing the build.
func TestStageSecretOptionalMissingSkips(t *testing.T) {
	e := newTestExecutor(t, "")
	e.ctx = context.Background()
	e.getSecret = func(ctx context.Context, id string) ([]byte, error) {
		return nil, fmt.Errorf("not found")
	}
	m := &pb.Mount{
		MountType: pb.MountType_SECRET,
		Dest:      "/x",
		SecretOpt: &pb.SecretOpt{ID: "missing", Optional: true},
	}
	path, err := e.stageSecret(t.TempDir(), m)
	if err != nil || path != "" {
		t.Fatalf("optional missing secret should skip: path=%q err=%v", path, err)
	}
}

// TestExecSourceLocalContext verifies local://context resolves to the FileSync'd
// build context directory.
func TestExecSourceLocalContext(t *testing.T) {
	ctxDir := t.TempDir()
	e := newTestExecutor(t, ctxDir)
	dir, err := e.execSource(&pb.SourceOp{Identifier: "local://context"})
	if err != nil {
		t.Fatalf("execSource: %v", err)
	}
	if dir != ctxDir {
		t.Fatalf("local://context = %q, want %q", dir, ctxDir)
	}
}

// TestSnapshotCopiesContents checks the dir-snapshot primitive used between ops.
func TestSnapshotCopiesContents(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := newTestExecutor(t, "")
	snap, err := e.snapshot(src)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap == src {
		t.Fatal("snapshot returned the source dir, expected a copy")
	}
	got, err := os.ReadFile(filepath.Join(snap, "f"))
	if err != nil || string(got) != "x" {
		t.Fatalf("snapshot content = %q err=%v, want x", got, err)
	}
}
