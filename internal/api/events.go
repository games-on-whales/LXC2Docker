package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
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
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func newEventBroker() *eventBroker {
	return &eventBroker{subs: make(map[chan Event]struct{})}
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
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
			// Subscriber is slow; drop this event for them rather than stall
			// every other subscriber and the emitting handler.
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
func (h *Handler) emitImage(action, ref string) {
	now := time.Now()
	h.events.publish(Event{
		Type:   "image",
		Action: action,
		Actor: EventActor{
			ID:         ref,
			Attributes: map[string]string{"name": ref},
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
	now := time.Now()
	image := rec.Image
	attrs := map[string]string{
		"image": image,
		"name":  rec.Name,
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

	// Parse optional since/until unix timestamps. If since is in the past
	// we have no backlog to replay (the broker is memory-only), so we just
	// honor until as a deadline for disconnecting.
	var until time.Time
	if s := r.URL.Query().Get("until"); s != "" {
		if ts, err := strconv.ParseInt(s, 10, 64); err == nil {
			until = time.Unix(ts, 0)
		}
	}
	filt := parseFilters(r)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := h.events.subscribe()
	defer h.events.unsubscribe(ch)

	// Heartbeat so idle Portainer connections don't time out on intermediate
	// proxies. Docker sends no heartbeat; we send a zero-byte flush which
	// the JSON decoder on the client ignores.
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	enc := json.NewEncoder(w)

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
	if f.has("container") {
		match := false
		for _, want := range f["container"] {
			if want == ev.Actor.ID || want == ev.Actor.Attributes["name"] {
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
	return true
}
