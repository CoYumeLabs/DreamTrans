package app

import "sync"

// Notifications are coalesced, not discarded state: clients fetch the latest
// committed snapshot whenever notified and on reconnect.
type hub struct {
	mu    sync.Mutex
	rooms map[string]map[chan struct{}]struct{}
}

func newHub() *hub { return &hub{rooms: make(map[string]map[chan struct{}]struct{})} }
func (h *hub) subscribe(code string) (chan struct{}, func(), bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.rooms[code]) >= 200 {
		return nil, nil, false
	}
	if h.rooms[code] == nil {
		h.rooms[code] = make(map[chan struct{}]struct{})
	}
	ch := make(chan struct{}, 1)
	h.rooms[code][ch] = struct{}{}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.rooms[code], ch)
		if len(h.rooms[code]) == 0 {
			delete(h.rooms, code)
		}
	}, true
}
func (h *hub) publish(code string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.rooms[code] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
