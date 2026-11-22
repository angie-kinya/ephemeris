// Package hub fans one terminal output stream out to many viewers
// Rule: the shell must never wait for a viewer. slow viewers get dropped
package hub

import "sync"

type Sub struct {
	ch   chan []byte
	once sync.Once
}

// C delivers output chunks; it is closed when the viewer is dropped or the hub closes
func (s *Sub) C() <-chan []byte { return s.ch }
func (s *Sub) close()           { s.once.Do(func() { close(s.ch) }) }

type Hub struct {
	mu     sync.Mutex
	subs   map[*Sub]struct{}
	replay []byte
	max    int
	closed bool
}

func New(replayBytes int) *Hub {
	return &Hub{subs: map[*Sub]struct{}{}, max: replayBytes}
}

func (h *Hub) Subscribe(buffer int) *Sub {
	s := &Sub{ch: make(chan []byte, max(buffer, 1))}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		s.close()
		return s
	}
	if len(h.replay) > 0 {
		s.ch <- append([]byte(nil), h.replay...) // fits: buffer >= 1 and s is new
	}
	h.subs[s] = struct{}{}
	return s
}

func (h *Hub) Unsubscribe(s *Sub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, s)
	s.close()
}

// write makes Hub an io.Writer: io.Copy(hub, ptyMaster) is the whole wiring
func (h *Hub) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.replay = append(h.replay, p...)
	if over := len(h.replay) - h.max; over > 0 {
		h.replay = append(h.replay[:0:0], h.replay[over:]...)
	}
	for s := range h.subs {
		select {
		case s.ch <- append([]byte(nil), p...): // copy: the caller reuses p
		default:
			delete(h.subs, s) // too slow: drop, never block the PTY reader
			s.close()
		}
	}
	return len(p), nil
}

func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for s := range h.subs {
		delete(h.subs, s)
		s.close()
	}
}
