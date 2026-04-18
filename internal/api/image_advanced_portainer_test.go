package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestImageConfigFromRecordExposesPortainerAdvancedMetadata(t *testing.T) {
	t.Parallel()

	cfg := imageConfigFromRecord(&store.ImageRecord{
		OCIMacAddress:      "02:42:ac:11:00:02",
		OCIArgsEscaped:     true,
		OCINetworkDisabled: true,
		OCIOnBuild:         []string{"RUN echo hi"},
		OCIShell:           []string{"/bin/bash", "-c"},
	})

	if cfg.MacAddress != "02:42:ac:11:00:02" {
		t.Fatalf("expected mac address, got %q", cfg.MacAddress)
	}
	if !cfg.ArgsEscaped || !cfg.NetworkDisabled {
		t.Fatalf("expected args-escaped/network-disabled flags, got %+v", cfg)
	}
	if len(cfg.OnBuild) != 1 || cfg.OnBuild[0] != "RUN echo hi" {
		t.Fatalf("expected onbuild, got %#v", cfg.OnBuild)
	}
	if len(cfg.Shell) != 2 || cfg.Shell[0] != "/bin/bash" {
		t.Fatalf("expected shell, got %#v", cfg.Shell)
	}
}

func TestApplyCommitConfigSupportsPortainerAdvancedMetadata(t *testing.T) {
	t.Parallel()

	mac := "02:42:ac:11:00:02"
	networkDisabled := true
	argsEscaped := true

	rec := &store.ImageRecord{}
	applyCommitConfig(rec, &commitConfig{
		MacAddress:      &mac,
		NetworkDisabled: &networkDisabled,
		ArgsEscaped:     &argsEscaped,
		OnBuild:         []string{"RUN echo hi"},
		Shell:           []string{"/bin/bash", "-c"},
	})

	if rec.OCIMacAddress != mac {
		t.Fatalf("expected mac address override, got %q", rec.OCIMacAddress)
	}
	if !rec.OCINetworkDisabled || !rec.OCIArgsEscaped {
		t.Fatalf("expected bool overrides, got %+v", rec)
	}
	if len(rec.OCIOnBuild) != 1 || rec.OCIOnBuild[0] != "RUN echo hi" {
		t.Fatalf("expected onbuild override, got %#v", rec.OCIOnBuild)
	}
	if len(rec.OCIShell) != 2 || rec.OCIShell[0] != "/bin/bash" {
		t.Fatalf("expected shell override, got %#v", rec.OCIShell)
	}
}

func TestSaveLoadRoundTripsPortainerAdvancedMetadata(t *testing.T) {
	t.Parallel()

	cfgBytes, _, err := synthesiseImageConfig(&store.ImageRecord{
		Arch:               "amd64",
		Created:            time.Unix(10, 0),
		OCIMacAddress:      "02:42:ac:11:00:02",
		OCIArgsEscaped:     true,
		OCINetworkDisabled: true,
		OCIOnBuild:         []string{"RUN echo hi"},
		OCIShell:           []string{"/bin/bash", "-c"},
	}, "layer")
	if err != nil {
		t.Fatalf("synthesise image config: %v", err)
	}

	var saved saveImageConfig
	if err := json.Unmarshal(cfgBytes, &saved); err != nil {
		t.Fatalf("decode saved config: %v", err)
	}
	effective := effectiveSaveImageConfig(saved)
	if effective.MacAddress != "02:42:ac:11:00:02" {
		t.Fatalf("expected saved mac address, got %q", effective.MacAddress)
	}
	if !effective.ArgsEscaped || !effective.NetworkDisabled {
		t.Fatalf("expected saved bool flags, got %+v", effective)
	}
	if len(effective.OnBuild) != 1 || effective.OnBuild[0] != "RUN echo hi" {
		t.Fatalf("expected saved onbuild, got %#v", effective.OnBuild)
	}
	if len(effective.Shell) != 2 || effective.Shell[0] != "/bin/bash" {
		t.Fatalf("expected saved shell, got %#v", effective.Shell)
	}
}
