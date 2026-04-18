package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestImageConfigFromRecordExposesPortainerConsoleMetadata(t *testing.T) {
	t.Parallel()

	timeout := 15
	cfg := imageConfigFromRecord(&store.ImageRecord{
		OCIHostname:     "demo",
		OCIDomainname:   "example.test",
		OCIUser:         "1000:1000",
		OCIAttachStdin:  true,
		OCIAttachStdout: true,
		OCIAttachStderr: true,
		OCITty:          true,
		OCIOpenStdin:    true,
		OCIStdinOnce:    true,
		OCIStopTimeout:  timeout,
	})

	if cfg.Hostname != "demo" || cfg.Domainname != "example.test" || cfg.User != "1000:1000" {
		t.Fatalf("expected hostname/domain/user to round-trip, got %+v", cfg)
	}
	if !cfg.AttachStdin || !cfg.AttachStdout || !cfg.AttachStderr || !cfg.Tty || !cfg.OpenStdin || !cfg.StdinOnce {
		t.Fatalf("expected console flags to round-trip, got %+v", cfg)
	}
	if cfg.StopTimeout == nil || *cfg.StopTimeout != timeout {
		t.Fatalf("expected stop timeout %d, got %+v", timeout, cfg.StopTimeout)
	}
}

func TestApplyCommitConfigSupportsPortainerConsoleMetadata(t *testing.T) {
	t.Parallel()

	hostname := "demo"
	domainname := "example.test"
	user := "1000:1000"
	attach := true
	tty := true
	openStdin := true
	stdinOnce := true
	workdir := "/workspace"
	stopSignal := "SIGQUIT"
	stopTimeout := 30

	rec := &store.ImageRecord{}
	applyCommitConfig(rec, &commitConfig{
		Hostname:     &hostname,
		Domainname:   &domainname,
		User:         &user,
		AttachStdin:  &attach,
		AttachStdout: &attach,
		AttachStderr: &attach,
		Tty:          &tty,
		OpenStdin:    &openStdin,
		StdinOnce:    &stdinOnce,
		WorkingDir:   &workdir,
		StopSignal:   &stopSignal,
		StopTimeout:  &stopTimeout,
	})

	if rec.OCIHostname != hostname || rec.OCIDomainname != domainname || rec.OCIUser != user {
		t.Fatalf("expected commit config hostname/domain/user, got %+v", rec)
	}
	if !rec.OCIAttachStdin || !rec.OCIAttachStdout || !rec.OCIAttachStderr || !rec.OCITty || !rec.OCIOpenStdin || !rec.OCIStdinOnce {
		t.Fatalf("expected commit config console flags, got %+v", rec)
	}
	if rec.OCIWorkingDir != workdir || rec.OCIStopSignal != stopSignal || rec.OCIStopTimeout != stopTimeout {
		t.Fatalf("expected commit config workdir/stop fields, got %+v", rec)
	}
}

func TestSaveLoadRoundTripsPortainerConsoleMetadata(t *testing.T) {
	t.Parallel()

	cfgBytes, _, err := synthesiseImageConfig(&store.ImageRecord{
		Arch:            "amd64",
		Created:         time.Unix(10, 0),
		OCIHostname:     "demo",
		OCIDomainname:   "example.test",
		OCIUser:         "1000:1000",
		OCIAttachStdin:  true,
		OCIAttachStdout: true,
		OCIAttachStderr: true,
		OCITty:          true,
		OCIOpenStdin:    true,
		OCIStdinOnce:    true,
		OCIStopTimeout:  45,
	}, "layer")
	if err != nil {
		t.Fatalf("synthesise image config: %v", err)
	}

	var saved saveImageConfig
	if err := json.Unmarshal(cfgBytes, &saved); err != nil {
		t.Fatalf("decode saved config: %v", err)
	}
	effective := effectiveSaveImageConfig(saved)
	if effective.Hostname != "demo" || effective.Domainname != "example.test" || effective.User != "1000:1000" {
		t.Fatalf("expected saved hostname/domain/user, got %+v", effective)
	}
	if !effective.AttachStdin || !effective.AttachStdout || !effective.AttachStderr || !effective.Tty || !effective.OpenStdin || !effective.StdinOnce {
		t.Fatalf("expected saved console flags, got %+v", effective)
	}
	if effective.StopTimeout == nil || *effective.StopTimeout != 45 {
		t.Fatalf("expected saved stop timeout, got %+v", effective.StopTimeout)
	}
}
