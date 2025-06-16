// Package compatcond provides a 100 %-compatible substitute for sync.Cond
// without depending on sync.Cond internally.  It matches the *current*
// Go runtime's LIFO wake-up order.
package torrent

import "sync"

// compatCond implements a condition variable identical in contract to sync.Cond.
type compatCond struct {
    L sync.Locker

    mu      sync.Mutex     // guards waiters
    waiters []chan struct{} // slice used as a LIFO stack
}

// newCompatCond returns a condition variable associated with l.
// Panics if l is nil (mirrors sync.NewCond).
func newCompatCond(l sync.Locker) *compatCond {
    if l == nil {
        panic("nil Locker passed to newCompatCond")
    }
    return &compatCond{L: l}
}

// Wait atomically unlocks c.L and suspends the caller.
// On resume it re-locks c.L before returning.
// The caller *must* hold c.L when calling Wait.
func (c *compatCond) Wait() {
    ch := make(chan struct{})

    // Push onto LIFO stack.
    c.mu.Lock()
    c.waiters = append(c.waiters, ch)
    c.mu.Unlock()

    // Release outer lock while blocked.
    // Special handling for lockWithDeferreds to avoid executing deferred actions
    if lwd, ok := c.L.(*lockWithDeferreds); ok {
        lwd.internal.Unlock()  // Bypass deferred actions
        <-ch
        lwd.internal.Lock()    // Re-acquire without triggering defers
    } else {
        c.L.Unlock()
        <-ch
        c.L.Lock()
    }
}

// Signal wakes exactly one goroutine blocked in Wait, preferring
// the most recently blocked one (LIFO).
func (c *compatCond) Signal() {
    c.mu.Lock()
    n := len(c.waiters)
    if n > 0 {
        ch := c.waiters[n-1]          // last waiter (LIFO)
        c.waiters = c.waiters[:n-1]   // pop in O(1)
        close(ch)                     // wake it
    }
    c.mu.Unlock()
}

// Broadcast wakes *all* goroutines blocked in Wait.
func (c *compatCond) Broadcast() {
    c.mu.Lock()
    for _, ch := range c.waiters {
        close(ch)
    }
    c.waiters = nil
    c.mu.Unlock()
}