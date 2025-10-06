# Webseed-peer.go Lock Analysis Summary

## Executive Summary

The recent changes to `webseed-peer.go` contain a **critical lock type mismatch** that violates the locking abstraction and could lead to incorrect behavior.

## The Problem

The `requester` function was changed to use internal locks:
```go
ws.peer.t.cl._mu.internal.Lock()    // Bypasses deferred actions
```

But it calls `requestIteratorLocked` which uses wrapper locks:
```go
ws.peer.t.cl.unlock()    // Executes deferred actions
ws.peer.t.cl.lock()      // May trigger deferred actions
```

This creates an **abstraction violation** - you cannot mix internal and wrapper lock operations on the same mutex.

## Why This Matters

The `lockWithDeferreds` type has two layers:
1. **Internal lock** (`_mu.internal`) - Raw mutex operations
2. **Wrapper methods** (`lock()/unlock()`) - Execute deferred actions on unlock

When you acquire with `internal.Lock()` but release with `unlock()`, you:
- Skip deferred actions that should run when acquiring the lock
- Execute deferred actions that weren't meant for this lock acquisition
- Break the invariant that deferred actions run exactly once per lock/unlock cycle

## Evidence from Git History

Looking at recent commits:
- `cd59cf60`: Introduced `compatCond` to fix sync.Cond compatibility issues with `lockWithDeferreds`
- `92ab34ec`: Started using internal locks in various places to avoid deadlocks
- Current change: Attempted to fix webseed deadlock but created lock type mismatch

## Correct Solution

Use internal locks consistently throughout the call chain:

```go
// In requestIteratorLocked, change all lock operations:
ws.peer.t.cl._mu.internal.Unlock()  // Line 107
ws.peer.t.cl._mu.internal.Lock()    // Line 111
ws.peer.t.cl._mu.internal.Unlock()  // Line 118  
ws.peer.t.cl._mu.internal.Lock()    // Line 123

// Also update requestResultHandler:
ws.peer.t.cl._mu.internal.Lock()    // Line 213
defer ws.peer.t.cl._mu.internal.Unlock()  // Line 214
```

## Verification Checklist

✅ All paths through `requestIteratorLocked` maintain lock held invariant  
✅ No double-lock issues exist  
❌ Lock types are NOT consistent (internal vs wrapper mismatch)  
✅ Pattern matches other internal lock usage in codebase  

## Risk Assessment

**Current Risk**: HIGH
- Potential for skipped deferred actions
- Possible lock state corruption
- Unpredictable behavior in production

**Recommended Action**: Fix immediately before deployment