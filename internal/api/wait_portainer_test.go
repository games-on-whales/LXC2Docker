package api

import (
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestWaitContainerResponseUsesRecordedExitCode(t *testing.T) {
	t.Parallel()

	out := waitContainerResponse(&store.ContainerRecord{ExitCode: 137})
	if got, ok := out["StatusCode"].(int); !ok || got != 137 {
		t.Fatalf("expected StatusCode 137, got %#v", out)
	}
	if out["Error"] != nil {
		t.Fatalf("expected nil Error, got %#v", out["Error"])
	}
}

func TestWaitContainerResponseDefaultsToZeroWithoutRecord(t *testing.T) {
	t.Parallel()

	out := waitContainerResponse(nil)
	if got, ok := out["StatusCode"].(int); !ok || got != 0 {
		t.Fatalf("expected StatusCode 0, got %#v", out)
	}
}
