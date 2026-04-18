package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// Event mirrors the Docker Engine event envelope (/events stream).
// Portainer and `docker events` consume these to invalidate caches and
// drive live UI updates.
type Event struct {
	Type     string     `json:"Type"`   // "container", "image", "network", "volume"
	Action   string     `json:"Action"` // "start", "die", "create", ...
	Actor    EventActor `json:"Actor"`
	Scope    string     `json:"scope"`    // "local"
	Time     int64      `json:"time"`     // unix seconds
	TimeNano int64      `json:"timeNano"` // unix ns
	// Legacy top-level fields — older clients (docker < 20.x) key off these.
	ID     string `json:"id"`
	Status string `json:"status"`
	From   string `json:"from"`
}

// EventActor is the nested actor object in a Docker event.
type EventActor struct {
	ID         string            `json:"ID"`
	Attributes map[string]string `json:"Attributes"`
}

// eventBroker fans out lifecycle events to any connected /events subscribers.
// Each subscriber owns a buffered channel; slow consumers drop events rather
// than blocking the emitter (the Docker daemon behaves the same way).
type eventBroker struct {
	mu      sync.Mutex
	subs    map[chan Event]struct{}
	history []Event
	histCap int
}

const localEventDaemon = "docker-lxc-daemon"

func newEventBroker() *eventBroker {
	return &eventBroker{
		subs:    make(map[chan Event]struct{}),
		history: make([]Event, 0, 1024),
		histCap: 1024,
	}
}

func (b *eventBroker) snapshotSince(since time.Time) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	if since.IsZero() {
		return nil
	}
	cutoff := since.UnixNano()
	var out []Event
	for _, ev := range b.history {
		if ev.TimeNano >= cutoff {
			out = append(out, ev)
		}
	}
	return out
}

func (b *eventBroker) subscribe() chan Event {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *eventBroker) unsubscribe(ch chan Event) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
	close(ch)
}

// publish emits an event to all current subscribers. Non-blocking: if a
// subscriber's channel is full the event is dropped for that client only.
func (b *eventBroker) publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.history) >= b.histCap {
		b.history = b.history[1:]
	}
	b.history = append(b.history, ev)
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// emitContainer is a convenience wrapper for the common case: a container
// lifecycle action on a stored record.
func (h *Handler) emitContainer(action string, rec *store.ContainerRecord) {
	h.emitContainerWithAttrs(action, rec, nil)
}

func (h *Handler) RestartEmitter() func(id, action string) {
	return func(id, action string) {
		h.emitContainer(action, h.store.GetContainer(id))
	}
}

// HealthEmitter returns a function suitable for lxc.Manager.StartHealthWatcher.
// Each call publishes a Docker-shaped "health_status" event (matching what
// `docker events` emits) so Portainer refreshes the container's health
// badge without a full snapshot refresh.
func (h *Handler) HealthEmitter() func(id, status string) {
	return func(id, status string) {
		rec := h.store.GetContainer(id)
		if rec == nil {
			return
		}
		h.emitContainerWithAttrs("health_status", rec, map[string]string{
			"health_status": status,
		})
		h.emitContainerWithAttrs("health_status: "+status, rec, map[string]string{
			"health_status": status,
		})
	}
}

// emitImage publishes an "image" event. Portainer subscribes to the events
// stream and refreshes its Images tab whenever it sees one. The actor ID
// is the ref (e.g. "nginx:latest") to match Docker's convention.
func (h *Handler) emitVolume(action, name string) {
	if h == nil || h.events == nil {
		return
	}
	now := time.Now()
	h.events.publish(Event{
		Type:   "volume",
		Action: action,
		Actor: EventActor{
			ID: name,
			Attributes: map[string]string{
				"daemon": localEventDaemon,
				"driver": "local",
				"name":   name,
				"type":   "volume",
			},
		},
		Scope:    "local",
		Time:     now.Unix(),
		TimeNano: now.UnixNano(),
		ID:       name,
		Status:   action,
		From:     name,
	})
}

func (h *Handler) emitNetwork(action, id, name string) {
	h.emitNetworkFull(action, id, name, "")
}

func (h *Handler) emitNetworkFull(action, id, name, containerID string) {
	if h == nil || h.events == nil {
		return
	}
	now := time.Now()
	attrs := map[string]string{
		"daemon":  localEventDaemon,
		"driver":  "bridge",
		"network": name,
		"scope":   "local",
		"type":    "network",
	}
	if name != "" {
		attrs["name"] = name
	}
	if containerID != "" {
		attrs["container"] = containerID
	}
	h.events.publish(Event{
		Type:   "network",
		Action: action,
		Actor: EventActor{
			ID:         id,
			Attributes: attrs,
		},
		Scope:    "local",
		Time:     now.Unix(),
		TimeNano: now.UnixNano(),
		ID:       id,
		Status:   action,
		From:     name,
	})
}

func (h *Handler) emitImage(action, ref string) {
	if h == nil || h.events == nil {
		return
	}
	now := time.Now()
	h.events.publish(Event{
		Type:   "image",
		Action: action,
		Actor: EventActor{
			ID: ref,
			Attributes: map[string]string{
				"daemon": localEventDaemon,
				"image":  ref,
				"name":   ref,
				"type":   "image",
			},
		},
		Scope:    "local",
		Time:     now.Unix(),
		TimeNano: now.UnixNano(),
		ID:       ref,
		Status:   action,
		From:     ref,
	})
}

// emitContainerWithAttrs emits a container lifecycle event with extra actor
// attributes merged in. Used by /rename to carry an "oldName" value so
// Portainer's event feed can render "foo renamed to bar".
func (h *Handler) emitContainerWithAttrs(action string, rec *store.ContainerRecord, extra map[string]string) {
	if rec == nil {
		return
	}
	if h == nil || h.events == nil {
		return
	}
	now := time.Now()
	image := rec.Image
	attrs := map[string]string{
		"container": rec.ID,
		"daemon":    localEventDaemon,
		"image":     image,
		"imageID":   rec.ImageID,
		"name":      rec.Name,
		"type":      "container",
	}
	if rec.ExitCode != 0 || action == "die" || action == "stop" || action == "kill" {
		attrs["exitCode"] = strconv.Itoa(rec.ExitCode)
	}
	for k, v := range rec.Labels {
		attrs[k] = v
	}
	for k, v := range extra {
		attrs[k] = v
	}
	h.events.publish(Event{
		Type:   "container",
		Action: action,
		Actor: EventActor{
			ID:         rec.ID,
			Attributes: attrs,
		},
		Scope:    "local",
		Time:     now.Unix(),
		TimeNano: now.UnixNano(),
		ID:       rec.ID,
		Status:   action,
		From:     image,
	})
}

// events implements GET /events. It streams JSON-encoded event objects
// newline-delimited until the client disconnects. Portainer keeps this
// connection open indefinitely and relies on it to drive incremental
// dashboard updates.
//
// Honored query params:
//   - since / until: unix timestamps; until disconnects the stream.
//   - filters: Docker filter map. Recognized keys are "container" (match
//     by container ID or name), "type" (container|image|network|volume),
//     "event" (specific action like "start"/"die"). Other keys are
//     ignored, matching Docker's laxity.
func (h *Handler) eventsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		errResponse(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	var until time.Time
	if s := r.URL.Query().Get("until"); s != "" {
		until = parseEventTimestamp(s)
	}
	var since time.Time
	if s := r.URL.Query().Get("since"); s != "" {
		since = parseEventTimestamp(s)
	}
	filt := parseFilters(r)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	enc := json.NewEncoder(w)
	if !since.IsZero() {
		for _, ev := range h.events.snapshotSince(since) {
			if !matchEvent(filt, ev) {
				continue
			}
			if !until.IsZero() && time.Unix(ev.Time, 0).After(until) {
				continue
			}
			if err := enc.Encode(&ev); err != nil {
				return
			}
		}
		flusher.Flush()
	}

	ch := h.events.subscribe()
	defer h.events.unsubscribe(ch)

	// Heartbeat so idle Portainer connections don't time out on intermediate
	// proxies. Docker sends no heartbeat; we send a zero-byte flush which
	// the JSON decoder on the client ignores.
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	var untilC <-chan time.Time
	if !until.IsZero() {
		t := time.NewTimer(time.Until(until))
		defer t.Stop()
		untilC = t.C
	}

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if !matchEvent(filt, ev) {
				continue
			}
			if err := enc.Encode(&ev); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			flusher.Flush()
		case <-untilC:
			return
		case <-r.Context().Done():
			return
		}
	}
}

// matchEvent applies the /events filter map to a single event. Returns true
// when the event should be delivered to the subscriber.
func matchEvent(f filters, ev Event) bool {
	if f.has("type") && !f.matchAny("type", ev.Type) {
		return false
	}
	if f.has("event") && !f.matchAny("event", ev.Action) {
		return false
	}
	if f.has("scope") && !f.matchAny("scope", ev.Scope) {
		return false
	}
	if f.has("container") {
		match := false
		for _, want := range f["container"] {
			name := strings.TrimPrefix(ev.Actor.Attributes["name"], "/")
			wantName := strings.TrimPrefix(want, "/")
			if want == ev.Actor.ID || want == ev.Actor.Attributes["name"] || wantName == name {
				match = true
				break
			}
			if len(want) >= 4 && strings.HasPrefix(ev.Actor.ID, want) {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	if f.has("label") && !f.matchLabel(ev.Actor.Attributes) {
		return false
	}
	if f.has("image") {
		img := ev.Actor.Attributes["image"]
		if img == "" {
			img = ev.From
		}
		if img == "" {
			img = ev.Actor.ID
		}
		if !f.matchAny("image", img) {
			return false
		}
	}
	if f.has("volume") {
		name := ev.Actor.Attributes["name"]
		if name == "" {
			name = ev.Actor.ID
		}
		if !f.matchAny("volume", name) && !f.matchAny("volume", ev.Actor.ID) {
			return false
		}
	}
	if f.has("network") {
		name := ev.Actor.Attributes["network"]
		if name == "" {
			name = ev.Actor.Attributes["name"]
		}
		if name == "" {
			name = ev.Actor.ID
		}
		if !f.matchAny("network", name) && !f.matchAny("network", ev.Actor.ID) {
			return false
		}
	}
	if f.has("daemon") {
		if !f.matchAny("daemon", ev.Actor.Attributes["daemon"]) {
			return false
		}
	}
	return true
}

func parseEventTimestamp(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if ts, err := strconv.ParseInt(raw, 10, 64); err == nil {
		switch {
		case len(raw) >= 19:
			return time.Unix(0, ts)
		case len(raw) >= 16:
			return time.Unix(0, ts*1000)
		case len(raw) >= 13:
			return time.Unix(0, ts*1_000_000)
		default:
			return time.Unix(ts, 0)
		}
	}
	if ts, err := strconv.ParseFloat(raw, 64); err == nil {
		sec := int64(ts)
		nsec := int64((ts - float64(sec)) * float64(time.Second))
		return time.Unix(sec, nsec)
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if ts, err := time.Parse(layout, raw); err == nil {
			return ts
		}
	}
	return time.Time{}
}
