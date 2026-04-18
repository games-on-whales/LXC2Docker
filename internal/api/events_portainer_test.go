package api

import (
	"testing"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
)

func TestParseEventTimestampAcceptsPortainerFormats(t *testing.T) {
	t.Parallel()

	want := time.Date(2026, 4, 18, 12, 34, 56, 789000000, time.UTC)
	tests := []string{
		want.Format(time.RFC3339Nano),
		want.Format(time.RFC3339),
		"1776515696",
		"1776515696.789",
		"1776515696789",
		"1776515696789000000",
	}
	for _, raw := range tests {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			got := parseEventTimestamp(raw)
			if got.IsZero() {
				t.Fatalf("expected parsed timestamp for %q", raw)
			}
		})
	}
}

func TestEmitContainerAddsPortainerAttributes(t *testing.T) {
	t.Parallel()

	h := &Handler{events: newEventBroker()}
	rec := &store.ContainerRecord{
		ID:       "abc123",
		Name:     "web",
		Image:    "docker.io/library/nginx:latest",
		ImageID:  "sha256:deadbeef",
		ExitCode: 137,
		Labels:   map[string]string{"com.docker.compose.project": "demo"},
	}

	h.emitContainer("die", rec)

	if len(h.events.history) != 1 {
		t.Fatalf("expected one event, got %d", len(h.events.history))
	}
	ev := h.events.history[0]
	for key, want := range map[string]string{
		"container": "abc123",
		"daemon":    localEventDaemon,
		"image":     "docker.io/library/nginx:latest",
		"imageID":   "sha256:deadbeef",
		"name":      "web",
		"type":      "container",
		"exitCode":  "137",
	} {
		if ev.Actor.Attributes[key] != want {
			t.Fatalf("expected %s=%q, got %#v", key, want, ev.Actor.Attributes)
		}
	}
}

func TestEmitNonContainerEventsAddPortainerAttributes(t *testing.T) {
	t.Parallel()

	h := &Handler{events: newEventBroker()}
	h.emitVolume("create", "data")
	h.emitNetworkFull("connect", "net-1", "frontend", "abc123")
	h.emitImage("tag", "docker.io/library/nginx:latest")

	if len(h.events.history) != 3 {
		t.Fatalf("expected three events, got %d", len(h.events.history))
	}

	vol := h.events.history[0]
	if vol.From != "data" || vol.Actor.Attributes["name"] != "data" || vol.Actor.Attributes["type"] != "volume" || vol.Actor.Attributes["daemon"] != localEventDaemon {
		t.Fatalf("unexpected volume event %#v", vol)
	}

	net := h.events.history[1]
	for key, want := range map[string]string{
		"network":   "frontend",
		"driver":    "bridge",
		"scope":     "local",
		"container": "abc123",
		"type":      "network",
		"daemon":    localEventDaemon,
	} {
		if net.Actor.Attributes[key] != want {
			t.Fatalf("expected %s=%q, got %#v", key, want, net.Actor.Attributes)
		}
	}
	if net.From != "frontend" {
		t.Fatalf("expected network From frontend, got %#v", net)
	}

	img := h.events.history[2]
	if img.Actor.Attributes["image"] != "docker.io/library/nginx:latest" || img.Actor.Attributes["type"] != "image" || img.Actor.Attributes["daemon"] != localEventDaemon {
		t.Fatalf("unexpected image event %#v", img)
	}
}

func TestMatchEventSupportsPortainerFilters(t *testing.T) {
	t.Parallel()

	ev := Event{
		Type:  "container",
		Scope: "local",
		From:  "docker.io/library/nginx:latest",
		Actor: EventActor{
			ID: "abc123456789",
			Attributes: map[string]string{
				"daemon": localEventDaemon,
				"image":  "docker.io/library/nginx:latest",
				"name":   "web",
				"type":   "container",
			},
		},
		Action: "start",
	}

	tests := []filters{
		{"container": {"/web"}},
		{"container": {"abc123"}},
		{"image": {"docker.io/library/nginx:latest"}},
		{"daemon": {localEventDaemon}},
	}
	for _, filt := range tests {
		if !matchEvent(filt, ev) {
			t.Fatalf("expected filter %#v to match event %#v", filt, ev)
		}
	}

	if !matchEvent(filters{"volume": {"data"}}, Event{
		Type: "volume",
		Actor: EventActor{ID: "data", Attributes: map[string]string{
			"name":   "data",
			"daemon": localEventDaemon,
		}},
	}) {
		t.Fatal("expected volume filter to match volume event")
	}
	if !matchEvent(filters{"network": {"frontend"}}, Event{
		Type: "network",
		Actor: EventActor{ID: "net-1", Attributes: map[string]string{
			"network": "frontend",
			"name":    "frontend",
			"daemon":  localEventDaemon,
		}},
	}) {
		t.Fatal("expected network filter to match network event")
	}
}
