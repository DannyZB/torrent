package torrent

import (
	"sync"
	anacrolixSync "github.com/anacrolix/sync"
)

// Runs deferred actions on Unlock. Note that actions are assumed to be the results of changes that
// would only occur with a write lock at present. The race detector should catch instances of defers
// without the write lock being held.
type lockWithDeferreds struct {
	internal      anacrolixSync.RWMutex
	unlockActions []func()
}

func (me *lockWithDeferreds) Lock() {
	me.internal.Lock()
}

func (me *lockWithDeferreds) Unlock() {
	unlockActions := me.unlockActions
	for i := 0; i < len(unlockActions); i += 1 {
		unlockActions[i]()
	}
	me.unlockActions = unlockActions[:0]
	me.internal.Unlock()
}

func (me *lockWithDeferreds) RLock() {
	me.internal.RLock()
}

func (me *lockWithDeferreds) RUnlock() {
	me.internal.RUnlock()
}

func (me *lockWithDeferreds) Defer(action func()) {
	me.unlockActions = append(me.unlockActions, action)
}

// SafeUnlock unlocks without executing deferred actions, for use with sync.Cond
func (me *lockWithDeferreds) SafeUnlock() {
	me.internal.Unlock()
}

// SafeLock is the counterpart to SafeUnlock
func (me *lockWithDeferreds) SafeLock() {
	me.internal.Lock()
}

// SafeLocker provides a sync.Locker interface that doesn't execute deferred actions
// This is safe to use with sync.Cond
type SafeLocker struct {
	mu *lockWithDeferreds
}

func (sl *SafeLocker) Lock() {
	sl.mu.SafeLock()
}

func (sl *SafeLocker) Unlock() {
	sl.mu.SafeUnlock()
}

// GetSafeLocker returns a sync.Locker that's safe to use with sync.Cond
func (me *lockWithDeferreds) GetSafeLocker() sync.Locker {
	return &SafeLocker{mu: me}
}

// FlushDeferred executes and clears all pending deferred actions while lock is held
// This is safe to call before operations that might trigger sync.Cond.Wait()
func (me *lockWithDeferreds) FlushDeferred() {
	unlockActions := me.unlockActions
	for i := 0; i < len(unlockActions); i += 1 {
		unlockActions[i]()
	}
	me.unlockActions = unlockActions[:0]
}
