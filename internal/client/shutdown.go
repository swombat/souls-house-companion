package client

import (
	"sync"
	"time"
)

// executeWorkers tracks only live execute workers, not filesystem operations.
// The connection loop is the sole caller of start and stops adding before wait.
// Workers remove themselves, so a long-lived connection retains at most the
// client's bounded worker capacity, not a channel for every completed request.
type executeWorkers struct {
	mu      sync.Mutex
	pending map[chan struct{}]struct{}
}

func (w *executeWorkers) start() func() {
	done := make(chan struct{})
	w.mu.Lock()
	if w.pending == nil {
		w.pending = make(map[chan struct{}]struct{})
	}
	w.pending[done] = struct{}{}
	w.mu.Unlock()
	return func() {
		w.mu.Lock()
		delete(w.pending, done)
		close(done)
		w.mu.Unlock()
	}
}

func (w *executeWorkers) wait(grace time.Duration) bool {
	w.mu.Lock()
	pending := make([]chan struct{}, 0, len(w.pending))
	for done := range w.pending {
		pending = append(pending, done)
	}
	w.mu.Unlock()
	if len(pending) == 0 {
		return true
	}
	// Independent of the canceled operation context, and one total deadline
	// even if several workers need cleanup. No unbounded Wait goroutine.
	timer := time.NewTimer(grace)
	defer timer.Stop()
	for _, done := range pending {
		select {
		case <-done:
		case <-timer.C:
			return false
		}
	}
	return true
}
