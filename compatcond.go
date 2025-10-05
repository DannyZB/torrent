package torrent

import "sync"

// compatCond provides a condition variable compatible with sync.Cond but built on top of
// lockWithDeferreds so that waits don't trigger deferred actions.
type compatCond struct {
	L sync.Locker

	mu      sync.Mutex
	waiters []chan struct{}
}

func newCompatCond(l sync.Locker) *compatCond {
	if l == nil {
		panic("nil Locker passed to newCompatCond")
	}
	return &compatCond{L: l}
}

func (c *compatCond) Wait() {
	ch := make(chan struct{})

	c.mu.Lock()
	c.waiters = append(c.waiters, ch)
	c.mu.Unlock()

	switch l := c.L.(type) {
	case *lockWithDeferreds:
		l.SafeUnlock()
		<-ch
		l.SafeLock()
	default:
		c.L.Unlock()
		<-ch
		c.L.Lock()
	}
}

func (c *compatCond) Signal() {
	c.mu.Lock()
	n := len(c.waiters)
	if n > 0 {
		ch := c.waiters[n-1]
		c.waiters = c.waiters[:n-1]
		close(ch)
	}
	c.mu.Unlock()
}

func (c *compatCond) Broadcast() {
	c.mu.Lock()
	for _, ch := range c.waiters {
		close(ch)
	}
	c.waiters = nil
	c.mu.Unlock()
}
