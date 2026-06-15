package api

import "testing"

// evaluateBuildStage must seed the build environment from the base image so the
// stage inherits HOME and the base PATH. Without this, `ENV PATH="$HOME/.cargo/
// bin:$PATH"` style instructions expand $HOME to empty and tools installed under
// $HOME (e.g. rustup) fall off PATH — the failure that broke building images
// FROM a base whose only HOME comes from inheritance.
func TestEvaluateBuildStage_InheritsBaseEnv(t *testing.T) {
	stage := dockerfileStage{
		baseRef: "example.com/base:latest",
		instructions: []dockerfileInstruction{
			{op: "ENV", args: "EXTRA=1"},
			{op: "ENV", args: "PATH=/opt/bin:/usr/bin"}, // overrides base PATH
		},
	}
	baseEnv := []string{"HOME=/home/retro", "PATH=/usr/bin:/bin", "LANG=C.UTF-8"}

	state, err := evaluateBuildStage(stage, baseEnv)
	if err != nil {
		t.Fatalf("evaluateBuildStage: %v", err)
	}

	if got := envValue(state.env, "HOME"); got != "/home/retro" {
		t.Errorf("HOME not inherited from base image: got %q, want /home/retro", got)
	}
	if got := envValue(state.env, "LANG"); got != "C.UTF-8" {
		t.Errorf("LANG not inherited from base image: got %q", got)
	}
	if got := envValue(state.env, "EXTRA"); got != "1" {
		t.Errorf("Dockerfile ENV not applied: got %q", got)
	}
	if got := envValue(state.env, "PATH"); got != "/opt/bin:/usr/bin" {
		t.Errorf("Dockerfile ENV should override base PATH: got %q", got)
	}
}

// A base that declares no environment (e.g. FROM scratch) still gets a usable
// default PATH so RUN steps can find core utilities.
func TestEvaluateBuildStage_DefaultPathWhenNoBaseEnv(t *testing.T) {
	stage := dockerfileStage{baseRef: "scratch"}
	state, err := evaluateBuildStage(stage, nil)
	if err != nil {
		t.Fatalf("evaluateBuildStage: %v", err)
	}
	if got := envValue(state.env, "PATH"); got == "" {
		t.Errorf("expected a default PATH when base declares none, got empty")
	}
}
