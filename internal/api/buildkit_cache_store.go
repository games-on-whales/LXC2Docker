package api

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/moby/buildkit/solver/pb"
	"github.com/opencontainers/go-digest"
)

// buildCacheSubdir holds persisted LLB op outputs under the daemon state dir.
const buildCacheSubdir = "buildkit-cache"

// isCacheable reports whether an op's output is fully content-addressed and may
// therefore be reused across builds. An op's own digest already encodes its
// whole input DAG, so the digest is a sound cache key — EXCEPT through a
// local://context source, which carries no hash of the actual context files.
// Hence an op is cacheable only when every source in its ancestry is a
// digest-pinned docker-image; anything depending on the build context is never
// cached, which avoids ever serving a stale result.
func (e *llbExecutor) isCacheable(dgst digest.Digest) bool {
	if v, ok := e.cacheableMemo[dgst]; ok {
		return v
	}
	op, ok := e.byDigest[dgst]
	if !ok {
		return false
	}
	res := false
	switch o := op.GetOp().(type) {
	case *pb.Op_Source:
		res = strings.HasPrefix(o.Source.GetIdentifier(), "docker-image://")
	default:
		res = true
		for _, in := range op.Inputs {
			if !e.isCacheable(digest.Digest(in.Digest)) {
				res = false
				break
			}
		}
	}
	e.cacheableMemo[dgst] = res
	return res
}

func (e *llbExecutor) cacheKeyDir(dgst digest.Digest) string {
	// digest is "alg:hex"; flatten to one safe directory name.
	return filepath.Join(e.cacheDir, strings.ReplaceAll(dgst.String(), ":", "_"))
}

// cacheGet returns snapshots of a cached op's outputs, or (nil,false) on miss.
// It returns fresh snapshots rather than the canonical cache dirs so downstream
// ops and the importer may mutate or move the result without corrupting the
// cache.
func (e *llbExecutor) cacheGet(dgst digest.Digest) (map[int64]string, bool) {
	if e.cacheDir == "" {
		return nil, false
	}
	base := e.cacheKeyDir(dgst)
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) == 0 {
		return nil, false
	}
	outs := map[int64]string{}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		var idx int64
		if _, err := fmt.Sscanf(ent.Name(), "%d", &idx); err != nil {
			continue
		}
		snap, err := e.snapshot(filepath.Join(base, ent.Name()))
		if err != nil {
			return nil, false
		}
		outs[idx] = snap
	}
	if len(outs) == 0 {
		return nil, false
	}
	e.emit(fmt.Sprintf("CACHED %s\n", short(dgst)))
	return outs, true
}

// cachePut stores an op's output dirs in the persistent build cache. Failures
// are non-fatal (the build already produced the result); the partial entry is
// removed so a later get can't see a half-written cache.
func (e *llbExecutor) cachePut(dgst digest.Digest, outs map[int64]string) {
	if e.cacheDir == "" {
		return
	}
	base := e.cacheKeyDir(dgst)
	tmp := base + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return
	}
	for idx, dir := range outs {
		dst := filepath.Join(tmp, fmt.Sprintf("%d", idx))
		if err := os.MkdirAll(dst, 0o755); err != nil {
			_ = os.RemoveAll(tmp)
			return
		}
		if out, err := exec.Command("cp", "-a", dir+"/.", dst).CombinedOutput(); err != nil {
			_ = os.RemoveAll(tmp)
			e.emit(fmt.Sprintf("warning: build cache store failed: %s\n", strings.TrimSpace(string(out))))
			return
		}
	}
	_ = os.RemoveAll(base)
	_ = os.Rename(tmp, base) // publish atomically
}

func short(dgst digest.Digest) string {
	s := dgst.String()
	if len(s) > 19 {
		return s[:19]
	}
	return s
}
