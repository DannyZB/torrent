package torrent

import (
	"fmt"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/missinggo/v2/panicif"
	xsync "github.com/anacrolix/sync"
)

// lockWithDeferreds wraps a RWMutex and runs deferred actions on Unlock.
type lockWithDeferreds struct {
	internal      xsync.RWMutex
	unlockActions []func()
	uniqueActions map[any]struct{}
	allowDefers   bool
	debug         *lockDebugState
}

func (me *lockWithDeferreds) Lock() {
	me.internal.Lock()
	panicif.True(me.allowDefers)
	me.allowDefers = true
	me.debugOnLock()
}

func (me *lockWithDeferreds) Unlock() {
	panicif.False(me.allowDefers)
	me.debugOnUnlock()
	me.allowDefers = false
	me.runUnlockActions()
	me.internal.Unlock()
}

func (me *lockWithDeferreds) RLock() {
	me.internal.RLock()
}

func (me *lockWithDeferreds) RUnlock() {
	me.internal.RUnlock()
}

// Defer schedules an action to run when the lock is unlocked.
func (me *lockWithDeferreds) Defer(action func()) {
	me.deferInner(action)
}

func (me *lockWithDeferreds) deferInner(action func()) {
	panicif.False(me.allowDefers)
	me.unlockActions = append(me.unlockActions, action)
}

func (me *lockWithDeferreds) deferOnceInner(key any, action func()) {
	panicif.False(me.allowDefers)
	g.MakeMapIfNil(&me.uniqueActions)
	if g.MapContains(me.uniqueActions, key) {
		return
	}
	me.uniqueActions[key] = struct{}{}
	me.deferInner(action)
}

// DeferUniqueUnaryFunc guards against duplicate scheduling of the same unary method.
func (me *lockWithDeferreds) DeferUniqueUnaryFunc(arg any, action func()) {
	me.deferOnceInner(unaryFuncKey(action, arg), action)
}

func unaryFuncKey(f func(), key any) funcAndArgKey {
	return funcAndArgKey{funcStr: reflect.ValueOf(f).String(), key: key}
}

type funcAndArgKey struct {
	funcStr string
	key     any
}

func (me *lockWithDeferreds) runUnlockActions() {
	startLen := len(me.unlockActions)
	for i := 0; i < len(me.unlockActions); i++ {
		me.unlockActions[i]()
	}
	if startLen != len(me.unlockActions) {
		panic(fmt.Sprintf("num deferred changed while running: %v -> %v", startLen, len(me.unlockActions)))
	}
	me.unlockActions = me.unlockActions[:0]
	me.uniqueActions = nil
}

// FlushDeferred executes pending actions while still holding the lock.
func (me *lockWithDeferreds) FlushDeferred() {
	panicif.False(me.allowDefers)
	me.runUnlockActions()
}

// SafeUnlock releases the internal mutex without running deferred actions (for compatCond).
func (me *lockWithDeferreds) SafeUnlock() {
	panicif.False(me.allowDefers)
	me.debugOnUnlock()
	me.allowDefers = false
	me.internal.Unlock()
}

// SafeLock reacquires the mutex after SafeUnlock.
func (me *lockWithDeferreds) SafeLock() {
	me.internal.Lock()
	panicif.True(me.allowDefers)
	me.allowDefers = true
	me.debugOnLock()
}

// SafeLocker yields a sync.Locker that uses SafeLock/SafeUnlock.
type SafeLocker struct {
	mu *lockWithDeferreds
}

func (sl *SafeLocker) Lock() {
	sl.mu.SafeLock()
}

func (sl *SafeLocker) Unlock() {
	sl.mu.SafeUnlock()
}

func (me *lockWithDeferreds) GetSafeLocker() sync.Locker {
	return &SafeLocker{mu: me}
}

// EnableDebug turns on ownership checks and optional stack capture for diagnostics.
func (me *lockWithDeferreds) EnableDebug(name string, captureStacks bool) {
	if name == "" && !captureStacks {
		me.debug = nil
		return
	}
	me.debug = &lockDebugState{
		name:          name,
		captureStacks: captureStacks,
	}
}

func (me *lockWithDeferreds) debugOnLock() {
	if me.debug == nil {
		return
	}
	gid := currentGoroutineID()
	if me.debug.owner == gid {
		me.debug.depth++
		return
	}
	if me.debug.owner != 0 {
		panic(fmt.Sprintf("lock %s already owned by goroutine %d (attempt %d)\nprevious lock stack:\n%s",
			me.debug.name,
			me.debug.owner,
			gid,
			strings.TrimSpace(string(me.debug.lastStack)),
		))
	}
	me.debug.owner = gid
	me.debug.depth = 1
	if me.debug.captureStacks {
		me.debug.lastStack = captureStack()
	}
}

func (me *lockWithDeferreds) debugOnUnlock() {
	if me.debug == nil {
		return
	}
	gid := currentGoroutineID()
	if me.debug.owner != gid {
		panic(fmt.Sprintf("unlock of %s by goroutine %d (owner %d)\nowner stack:\n%s",
			me.debug.name,
			gid,
			me.debug.owner,
			strings.TrimSpace(string(me.debug.lastStack)),
		))
	}
	me.debug.depth--
	if me.debug.depth == 0 {
		me.debug.owner = 0
		if me.debug.captureStacks {
			me.debug.lastStack = nil
		}
	}
}

type lockDebugState struct {
	name          string
	owner         int64
	depth         int
	captureStacks bool
	lastStack     []byte
}

func captureStack() []byte {
	buf := make([]byte, 2048)
	for {
		n := runtime.Stack(buf, false)
		if n < len(buf) {
			return buf[:n]
		}
		buf = make([]byte, len(buf)*2)
	}
}

// DebugInfo returns a human-readable string describing the current lock holder.
// Safe to call concurrently (reads are racy but acceptable for diagnostics).
// Returns empty string if debug is not enabled or lock is not held.
func (me *lockWithDeferreds) DebugInfo() string {
	d := me.debug
	if d == nil {
		return "debug not enabled (set TORRENT_LOCK_DEBUG=stack)"
	}
	owner := d.owner
	if owner == 0 {
		return "lock not held"
	}
	stack := string(d.lastStack)
	if stack == "" {
		return fmt.Sprintf("lock %q held by goroutine %d (no stack captured, set TORRENT_LOCK_DEBUG=stack)", d.name, owner)
	}
	return fmt.Sprintf("lock %q held by goroutine %d\n%s", d.name, owner, stack)
}

func currentGoroutineID() int64 {
	const prefix = "goroutine "
	buf := make([]byte, 64)
	n := runtime.Stack(buf, false)
	line := strings.TrimPrefix(string(buf[:n]), prefix)
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return -1
	}
	id, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return -1
	}
	return id
}
