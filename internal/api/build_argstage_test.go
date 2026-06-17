package api

import (
	"context"
	"strconv"
	"testing"
)

// argStageDockerfile mirrors the shape real multi-stage images (e.g. aimee's
// server/combined Dockerfiles) rely on: an ARG selects which prior stage a later
// `FROM` resolves to (FROM sel-${WITH_EXTRA}), and the final stage pulls files
// from prior stages via COPY --from. With WITH_EXTRA=1 the selector picks the
// stage that depends on extra-build; with =0 it picks an empty stage and
// extra-build prunes out of the graph entirely.
const argStageDockerfile = `
ARG WITH_EXTRA=1
FROM busybox AS base
RUN echo base > /a
FROM busybox AS extra-build
RUN echo extra > /x
FROM busybox AS sel-1
COPY --from=extra-build /x /x
FROM busybox AS sel-0
FROM sel-${WITH_EXTRA} AS sel
FROM busybox AS final
COPY --from=base /a /a
COPY --from=sel /x /y
`

// TestBuildKitArgDrivenStageSelectionPrunes proves the BuildKit LLB path
// (docker buildx) resolves an ARG-interpolated `FROM sel-${ARG}` and prunes the
// unselected branch: WITH_EXTRA=0 must yield a strictly smaller op DAG than =1
// because extra-build (and the COPY that pulls from it) drops out.
func TestBuildKitArgDrivenStageSelectionPrunes(t *testing.T) {
	df := []byte(argStageDockerfile)
	count := func(wc string) int {
		def, img, err := dockerfileToLLB(context.Background(), df, "",
			map[string]string{"WITH_EXTRA": wc}, map[string]string{}, fakeMetaResolver{})
		if err != nil {
			t.Fatalf("WITH_EXTRA=%s: dockerfileToLLB: %v", wc, err)
		}
		if img == nil || len(def.Def) == 0 {
			t.Fatalf("WITH_EXTRA=%s: empty LLB/img", wc)
		}
		verts, _, err := llbOps(def)
		if err != nil {
			t.Fatalf("WITH_EXTRA=%s: llbOps: %v", wc, err)
		}
		return len(verts)
	}
	on, off := count("1"), count("0")
	t.Logf("LLB vertices: WITH_EXTRA=1 -> %d, WITH_EXTRA=0 -> %d", on, off)
	if on <= off {
		t.Errorf("expected the selected branch (=1) to add vertices over the pruned one (=0); got on=%d off=%d", on, off)
	}
}

// TestClassicArgDrivenStageResolvesToPriorStage proves the classic POST /build
// path substitutes ARGs before splitting stages, so `FROM sel-${WITH_EXTRA}`
// becomes `FROM sel-1`, which resolves to a previously built stage instead of
// being pulled as a registry image.
func TestClassicArgDrivenStageResolvesToPriorStage(t *testing.T) {
	for _, tc := range []struct{ wc, wantBase string }{
		{"1", "sel-1"},
		{"0", "sel-0"},
	} {
		instrs, err := parseDockerfile(argStageDockerfile)
		if err != nil {
			t.Fatalf("WITH_EXTRA=%s: parseDockerfile: %v", tc.wc, err)
		}
		argSet := map[string]string{"WITH_EXTRA": tc.wc}
		for i := range instrs {
			if instrs[i].op != "ARG" {
				instrs[i].args = substituteBuildArgs(instrs[i].args, argSet)
			}
		}
		stages, err := splitDockerfileStages(instrs)
		if err != nil {
			t.Fatalf("WITH_EXTRA=%s: splitDockerfileStages: %v", tc.wc, err)
		}
		stageRefs := map[string]string{}
		var sel *dockerfileStage
		for idx := range stages {
			s := &stages[idx]
			stageRefs[strconv.Itoa(idx)] = "ref-" + strconv.Itoa(idx)
			if s.name != "" {
				stageRefs[s.name] = "ref-" + s.name
			}
			if s.name == "sel" {
				sel = s
			}
		}
		if sel == nil {
			t.Fatalf("WITH_EXTRA=%s: no `sel` stage parsed", tc.wc)
		}
		if sel.baseRef != tc.wantBase {
			t.Errorf("WITH_EXTRA=%s: sel base = %q, want %q (ARG substitution)", tc.wc, sel.baseRef, tc.wantBase)
		}
		if got := resolveStageBaseRef(sel.baseRef, stageRefs); got == sel.baseRef {
			t.Errorf("WITH_EXTRA=%s: sel base %q did not resolve to a prior stage", tc.wc, sel.baseRef)
		}
		if _, err := selectBuildTarget(stages, ""); err != nil {
			t.Errorf("WITH_EXTRA=%s: selectBuildTarget(default): %v", tc.wc, err)
		}
	}
}
