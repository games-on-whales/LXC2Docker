package api

import "sync"

type eventHub struct {
	mu          sync.RWMutex
	subscribers map[chan EventMessage]struct{}
}

func newEventHub() *eventHub {
	return &eventHub{subscribers: make(map[chan EventMessage]struct{})}
}

func (h *eventHub) subscribe() chan EventMessage {
	ch := make(chan EventMessage, 32)
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *eventHub) unsubscribe(ch chan EventMessage) {
	h.mu.Lock()
	if _, ok := h.subscribers[ch]; ok {
		delete(h.subscribers, ch)
		close(ch)
	}
	h.mu.Unlock()
}

func (h *eventHub) publish(evt EventMessage) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subscribers {
		select {
		case ch <- evt:
		default:
		}
	}
}
