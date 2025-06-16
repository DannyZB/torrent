package torrent

import "sync"

// Event provides condition variable functionality that's compatible with lockWithDeferreds.
// It replaces sync.Cond to avoid deadlocks when used with custom mutex implementations
// that execute deferred actions during Unlock().
type Event struct {
	mu      sync.Mutex
	waiters []chan struct{}
}

// Wait blocks until Broadcast is called, properly releasing and re-acquiring the provided mutex.
// This is equivalent to sync.Cond.Wait() but safe to use with lockWithDeferreds.
func (e *Event) Wait(clientMu sync.Locker) {
	// Register while holding client lock - prevents race
	e.mu.Lock()
	ch := make(chan struct{})
	e.waiters = append(e.waiters, ch)
	e.mu.Unlock()

	// Release client lock and wait
	clientMu.Unlock()
	<-ch
	clientMu.Lock()
}

// Broadcast wakes all goroutines waiting on this Event.
// This is equivalent to sync.Cond.Broadcast().
func (e *Event) Broadcast() {
	e.mu.Lock()
	waiters := e.waiters
	e.waiters = nil // Clear for next round
	e.mu.Unlock()

	// Wake all waiters
	for _, ch := range waiters {
		close(ch)
	}
}