package api

import "testing"

func TestSplitDockerfileStagesSupportsPortainerMultiStageDockerfiles(t *testing.T) {
	t.Parallel()

	instrs := []dockerfileInstruction{
		{op: "ARG", args: "BASE=alpine", line: 1},
		{op: "FROM", args: "--platform=linux/amd64 ${BASE} AS builder", line: 2},
		{op: "RUN", args: "echo build", line: 3},
		{op: "FROM", args: "scratch AS final", line: 4},
		{op: "COPY", args: "--from=builder /out /", line: 5},
	}

	stages, err := splitDockerfileStages(instrs)
	if err != nil {
		t.Fatalf("split stages: %v", err)
	}
	if len(stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(stages))
	}
	if stages[0].baseRef != "${BASE}" || stages[0].name != "builder" {
		t.Fatalf("unexpected first stage %+v", stages[0])
	}
	if stages[1].baseRef != "scratch" || stages[1].name != "final" {
		t.Fatalf("unexpected second stage %+v", stages[1])
	}
}

func TestSelectBuildTargetSupportsNamesAndIndexes(t *testing.T) {
	t.Parallel()

	stages := []dockerfileStage{{name: "builder"}, {name: "final"}}
	if idx, err := selectBuildTarget(stages, "final"); err != nil || idx != 1 {
		t.Fatalf("expected named target final -> 1, got idx=%d err=%v", idx, err)
	}
	if idx, err := selectBuildTarget(stages, "0"); err != nil || idx != 0 {
		t.Fatalf("expected numeric target 0 -> 0, got idx=%d err=%v", idx, err)
	}
	if idx, err := selectBuildTarget(stages, ""); err != nil || idx != 1 {
		t.Fatalf("expected default target -> last stage, got idx=%d err=%v", idx, err)
	}
}

func TestParseCopyInstructionSupportsFromOption(t *testing.T) {
	t.Parallel()

	from, srcs, dest, err := parseCopyInstruction("--from=builder /out/app /app")
	if err != nil {
		t.Fatalf("parse copy: %v", err)
	}
	if from != "builder" || len(srcs) != 1 || srcs[0] != "/out/app" || dest != "/app" {
		t.Fatalf("unexpected parsed copy: from=%q srcs=%#v dest=%q", from, srcs, dest)
	}

	from, srcs, dest, err = parseCopyInstruction("--chown=1000:1000 --from 1 /src /dst")
	if err != nil {
		t.Fatalf("parse copy with spaced from: %v", err)
	}
	if from != "1" || len(srcs) != 1 || srcs[0] != "/src" || dest != "/dst" {
		t.Fatalf("unexpected parsed copy with spaced from: from=%q srcs=%#v dest=%q", from, srcs, dest)
	}
}

func TestEvaluateBuildStagePreservesPortainerMetadata(t *testing.T) {
	t.Parallel()

	stage := dockerfileStage{
		baseRef: "docker.io/library/alpine:latest",
		instructions: []dockerfileInstruction{
			{op: "WORKDIR", args: "/app"},
			{op: "ENV", args: "A=B"},
			{op: "CMD", args: `["serve"]`},
			{op: "ENTRYPOINT", args: `["/init"]`},
			{op: "EXPOSE", args: "8080/tcp 8443/tcp"},
			{op: "LABEL", args: `role=web`},
			{op: "USER", args: "1000:1000"},
			{op: "STOPSIGNAL", args: "SIGTERM"},
			{op: "VOLUME", args: `["/data"]`},
			{op: "SHELL", args: `["/bin/bash","-c"]`},
		},
	}

	state, err := evaluateBuildStage(stage)
	if err != nil {
		t.Fatalf("evaluate stage: %v", err)
	}
	if state.baseRef != "docker.io/library/alpine:latest" || state.workdir != "/app" || state.user != "1000:1000" || state.stopSignal != "SIGTERM" {
		t.Fatalf("unexpected state metadata %+v", state)
	}
	if len(state.cmd) != 1 || state.cmd[0] != "serve" {
		t.Fatalf("unexpected cmd %+v", state.cmd)
	}
	if len(state.entrypoint) != 1 || state.entrypoint[0] != "/init" {
		t.Fatalf("unexpected entrypoint %+v", state.entrypoint)
	}
	if len(state.exposed) != 2 || len(state.volumes) != 1 || len(state.shell) != 2 {
		t.Fatalf("unexpected stage fields %+v", state)
	}
	if state.labels["role"] != "web" {
		t.Fatalf("expected label role=web, got %+v", state.labels)
	}
}

func TestBuildRunArgsHonorsDockerfileUserForPortainerBuilds(t *testing.T) {
	t.Parallel()

	args := buildRunArgs("/tmp/rootfs", []string{"/bin/sh", "-lc"}, "1000:1000", "echo hi")
	want := []string{"--userspec", "1000:1000", "/tmp/rootfs", "/bin/sh", "-lc", "echo hi"}
	if len(args) != len(want) {
		t.Fatalf("expected %d args, got %d: %#v", len(want), len(args), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("expected arg %d = %q, got %#v", i, want[i], args)
		}
	}

	args = buildRunArgs("/tmp/rootfs", []string{"/bin/sh", "-lc"}, "", "echo hi")
	if len(args) < 1 || args[0] != "/tmp/rootfs" {
		t.Fatalf("expected rootfs first when no user is set, got %#v", args)
	}
}

func TestIsScratchBuildRefSupportsPortainerScratchStages(t *testing.T) {
	t.Parallel()

	if !isScratchBuildRef("scratch") {
		t.Fatal("expected scratch to be recognized")
	}
	if !isScratchBuildRef(" Scratch ") {
		t.Fatal("expected scratch matching to be case-insensitive and trimmed")
	}
	if isScratchBuildRef("docker.io/library/scratch:latest") {
		t.Fatal("expected only literal scratch to be treated as a scratch stage")
	}
}
