# Torrent Library Fixes - October 6-7, 2025

## Context

On **October 6, 2025 08:58**, upstream anacrolix/torrent was merged into the fork (commit `edb0a458`). This introduced multiple critical issues that caused production outages across the fleet (rd6, rd25, nw4, nw23).

## Issues Fixed

### 1. Recursive Lock Deadlocks in Webseed (3 commits)

**Root Cause:** `ws.locker` is set to the Client lock (`torrent.go:3197`). Code paths that already held the Client lock tried to acquire it again.

#### Fix 1: `enqueueRequestSpawn()` - Commit `0e55c00b`
- **Problem:** Called from `updateWebseedRequests` which holds Client lock
- **Deadlock:** `ws.locker.Lock()` = recursive lock acquisition
- **Fix:** Removed lock/unlock - caller already holds it
- **File:** `webseed-peer.go:177-189`

#### Fix 2: `closeRequesters()` - Commit `6a8c734f`
- **Problem:** Called from `Drop → Torrent.close → Peer.close → onClose`
- **Deadlock:** `ws.locker.Lock()` while Drop holds Client lock
- **Fix:** Removed lock/unlock at lines 202-204
- **File:** `webseed-peer.go:192-207`

#### Fix 3: `onClose()` - Commit `ffafe3db`
- **Problem:** Same call path as above
- **Deadlock:** Second lock in same function at lines 406-408
- **Fix:** Removed lock around `ws.requestQueue = nil`
- **File:** `webseed-peer.go:404-415`

**Key Insight:** All three were trying to lock `ws.locker` which IS the Client lock, while already holding it.

### 2. Performance Bottleneck - Commit `3a67dbf9`

**Problem:** `checkPendingPiecesMatchesRequestOrder()` added in upstream commit `414bd578` (Sept 25, 2025)
- Iterates ALL pieces in btree to validate consistency
- Called from `needData()` while holding Client lock
- For 333k+ piece torrents, blocked Client lock for extended periods
- **nw4:** 1 goroutine doing CPU work, 559 blocked waiting

**Fix:** Gated behind `debugMetricsEnabled` flag (controlled by `ClientConfig.Debug`)
- **File:** `torrent.go:1856-1858`
- Default: check disabled
- With debug: check runs

### 3. Webseed Panic - Commit `29094ff5`

**Problem:** `MapMustAssignNew` panics when key already exists
- Multiple webseed requester goroutines run concurrently
- Different (begin, end) ranges can map to same sliceIndex
- Both try to add to `cl.activeWebseedRequests` → panic

**Stack Trace:**
```
panic: torrent "10glava-170": webseed /28/items/: slice 2
MapMustAssignNew at webseed-peer.go:278
```

**Fix:** Check-and-replace pattern instead of MapMustAssignNew
- If key exists, cancel existing request before replacing
- **File:** `webseed-peer.go:278-285`

### 4. Option Panic - Commit `a06acae6`

**Problem:** `getFileByPiecesRoot()` calls `Unwrap()` on unset Option
- `piecesRoot` only set for v2 torrents with per-file hashes
- `Option.Unwrap()` panics if `Ok == false`

**Stack Trace:**
```
panic: not set
github.com/anacrolix/generics.Option[...].Unwrap(...)
github.com/anacrolix/torrent.(*Torrent).getFileByPiecesRoot(...) torrent.go:3577
```

**Fix:** Check `Option.Ok` before accessing `Option.Value`
- **File:** `torrent.go:3577`
- Changed: `if f.piecesRoot.Unwrap() == hash`
- To: `if f.piecesRoot.Ok && f.piecesRoot.Value == hash`

### 5. Download Stalling - Commit `aa84070e` ⚠️ **CRITICAL**

**Problem Chain:**

1. **Commit `523f2b66`** (Oct 6 19:34): Changed `File.SetPriority` to use `internal.Lock` for performance
   - Sets `allowDefers=false` to bypass deferred actions

2. **Call chain:**
   ```
   File.SetPriority (allowDefers=false)
     → updatePiecePriorities
       → updatePiecePriority
         → updatePeerRequestsForPiece
           → onNeedUpdateRequests (torrent.go:1532)
   ```

3. **Commit `974173cb`** (Oct 6 20:22): Tried to fix panics when `allowDefers=false`
   - Called `handleOnNeedUpdateRequests()` directly
   - **Result:** DEADLOCK (from webseed readChunks + onClose paths)

4. **Commit `a3b378b0`** (Oct 6 22:24): Fixed deadlock by skipping entirely
   - When `allowDefers=false`: skip request updates
   - **Result:** Downloads STALLED (needRequestUpdate set but writer never woke)

5. **Commit `aa84070e`** (Oct 7 05:26): **FINAL FIX**
   - Call `tickleWriter()` when `allowDefers=false`
   - Writer wakes, waits for lock, processes `needRequestUpdate` flag
   - Safe now because webseed/onClose deadlocks already fixed

**Why This Fix Is Safe:**
- `tickleWriter()` → `Broadcast()` is non-blocking
- Writer wakes in separate goroutine, waits for lock to be released
- Writer then calls `fillWriteBuffer()` → `maybeUpdateActualRequestState()`
- Previous deadlocks were from webseed `readChunks` (fixed `aa5c99bb`) and `onClose` paths (fixed `6a8c734f`, `ffafe3db`)

**File:** `peerconn.go:1809-1819`

## Understanding the Lock System

### Regular Lock (`cl.lock()` / `cl.unlock()`)
- Sets `allowDefers=true`
- Runs deferred actions on unlock
- Used for normal operations

### Internal Lock (`cl._mu.internal.Lock()`)
- Sets `allowDefers=false`
- Bypasses deferred actions
- Used in:
  - `File.SetPriority` (file.go:198)
  - `dialAndCompleteHandshake` holepunch paths (client.go:878, 891)
  - Performance-critical paths

### Why Internal Lock Exists
- Avoids accumulating deferred actions in tight loops
- Prevents recursive deferrals
- Performance optimization

### Deferred Actions Pattern
```go
// Regular lock - can defer
cl.lock()
cl._mu.DeferUniqueUnaryFunc(cn, cn.handleOnNeedUpdateRequests)
cl.unlock() // Runs deferred actions here

// Internal lock - cannot defer (would panic)
cl._mu.internal.Lock()
// Must handle immediately or set flag for later
cn.needRequestUpdate = reason
cn.tickleWriter() // Wake writer to process flag
cl._mu.internal.Unlock()
```

## Git History

```
aa84070e Fix download stalling: tickle writer when allowDefers=false
a06acae6 Fix panic in getFileByPiecesRoot: check Option.Ok before accessing Value
29094ff5 Fix webseed panic: handle duplicate sliceIndex gracefully
3a67dbf9 Gate expensive validation check behind debug flag
ffafe3db Fix another recursive lock deadlock in onClose
6a8c734f Fix recursive lock deadlock in closeRequesters
0e55c00b Fix critical recursive lock deadlock in webseed request spawning
a3b378b0 Fix deadlock in onNeedUpdateRequests: skip when allowDefers=false [REVERTED BY aa84070e]
89bf277b Fix deadlock: revert direct publishStateChange() call in internal lock context
aa5c99bb fixes [Reverted webseed readChunks internal lock]
974173cb lock fix [CAUSED DEADLOCKS - fixed by a3b378b0, then aa84070e]
a97a4c2d locks fix
523f2b66 critical lock contention fixes [Added internal locks]
afa30f54 fix
edb0a458 Merge branch 'merge/upstream-integration' [THE MERGE]
f765d0d1 Add support for passive readers and improve request handling
```

## Key Files Modified

1. **webseed-peer.go**
   - Removed recursive locks in 3 places
   - Fixed MapMustAssignNew panic

2. **torrent.go**
   - Gated expensive validation behind debug flag
   - Fixed Option.Unwrap panic

3. **peerconn.go**
   - Final fix for request update stalling

4. **file.go**
   - Still uses internal lock (for performance)
   - Works because peerconn.go now handles allowDefers=false correctly

## ⚠️ PARTIAL FIX - Download Stalling Fix - Commit `ac32e79e` (Oct 7, 2025)

### The Fix: Request Updates Must Run Even When Buffer Full

**Status:**
- ✅ **Standalone anacrolix/torrent library: WORKING** (127 MB in 45s @ 11 MB/s)
- ❌ **seedr_subnode torrentclient wrapper: BROKEN** (0 B/s, no downloads)

**Root Cause (Fixed in standalone):**
`fillWriteBuffer()` had an early return when buffer > 65KB that prevented request updates from running.

**The Problem Chain:**
1. `File.SetPriority` uses `internal.Lock` (allowDefers=false) for performance
2. Commit `aa84070e` correctly added `tickleWriter()` call to wake the writer
3. Writer wakes and calls `fillWriteBuffer()`
4. **BUG**: If write buffer > 65KB, function returned early (line 405)
5. `maybeUpdateActualRequestState()` never got called (was at line 414)
6. `needRequestUpdate` flag stayed set but was never processed
7. Downloads stalled forever - no piece requests sent

**The Fix (Commit ac32e79e):**
Move request update calls BEFORE the buffer size check in `peerconn.go:fillWriteBuffer()`:
```go
func (cn *PeerConn) fillWriteBuffer() {
    // NEW: Always process request updates first - critical for download progress
    cn.requestMissingHashes()
    cn.maybeUpdateActualRequestState()

    if cn.messageWriter.writeBuffer.Len() > writeBufferLowWaterLen {
        return  // Now safe - we already updated requests
    }
    // ... pex and upload
}
```

**Why This Works:**
- Both functions have cheap early-exit guards (string check, bool check)
- Only run expensive work when state actually changed (event-driven)
- No performance regression - same work, different order
- Ensures critical request updates always happen, even when buffer is full

**Test Results:**

**Standalone Library Test** (`/tmp/torrent-test download`)
```bash
Test: Bob's Burgers S16E02 (176 MB torrent)
Time: 45 seconds
Progress: 38 MB → 165 MB (127 MB downloaded)
Speeds: 2.5 MB/s → 8.9 MB/s → 12 MB/s → 11 MB/s
Result: ✅ WORKING
Command: /tmp/torrent-test download --stats --no-seed <magnet>
```

**seedr_subnode Test** (`src/download/test/torrent.go`)
```bash
Test: Bob's Burgers S16E02 (176 MB torrent)
Time: 50 seconds
Progress: 0 B (0%)
Speed: 0 B/s (constant)
Peers: S:1 L:49 (1 seeder, 49 leechers available)
Result: ❌ STALLED - NO DOWNLOAD ACTIVITY
Command: go run torrent.go <magnet>
Called: t.DownloadAll() - still no downloads
```

**Fix Chain Status:**
1. ✅ Fixed recursive lock deadlocks (0e55c00b, 6a8c734f, ffafe3db)
2. ✅ Fixed performance bottleneck (3a67dbf9)
3. ✅ Fixed panics (29094ff5, a06acae6)
4. ✅ Fixed writer wake on internal lock (aa84070e)
5. ✅ Fixed request updates when buffer full (ac32e79e) - **Standalone works**
6. ❌ **seedr_subnode still broken** - Different issue in torrentclient wrapper

## 🔴 CRITICAL REGRESSION - `DisableInitialPieceCheck` Breaks Downloads (Oct 7, 2025)

### Current Status

**The Issue:** Setting `DisableInitialPieceCheck=true` in AddTorrentOpts completely breaks downloads in the new library version. ALL pieces get Priority=None and are ignored for requests.

**Affected Code:** `seedr_subnode/src/download/torrentclient/add.go` sets this flag in 3 places:
- Line 38: `AddMagnet()`
- Line 100: `AddFromFile()`
- Line 149: `AddFromBytes()`

### Test Results

**WITH DisableInitialPieceCheck=true (BROKEN):**
```
Test directory: /home/daniel/tmp/seedr-torrent-test-1759822910
Got info: Bobs-Burgers-S16E02-HDTV-X264.mp4 (1 files)
Starting download (will run for 45 seconds)...

ActivePeers: 1
ConnectedSeeders: 1
BytesReadData: 0  ← NO DATA DOWNLOADED
BytesWrittenData: 0

NumPieces: 672
Piece State Runs:
  Run 0: Priority=0, Partial=false, Checking=false, Complete=false, Length=672
  Summary: 672 pieces with Priority=None, 0 pieces with Priority>0  ← ALL PIECES PRIORITY=NONE

File Info:
  File Priority: 1  ← File has priority
  Piece priorities: 672 with None, 0 with Normal  ← But pieces don't!

Result: 0 B downloaded, 0 B/s, 0% complete
```

**WITHOUT DisableInitialPieceCheck (WORKING):**
```
Test directory: /home/daniel/tmp/seedr-torrent-test-1759823839
Got info: Bobs-Burgers-S16E02-HDTV-X264.mp4 (1 files)
Starting download (will run for 45 seconds)...

ActivePeers: 5
ConnectedSeeders: 5
BytesReadData: 9617408  ← 9.6 MB DOWNLOADED
BytesWrittenData: 0

NumPieces: 672
Piece State Runs:
  Run 0: Priority=1, Partial=true, Checking=false, Complete=false, Length=1
  Run 1: Priority=1, Partial=false, Checking=false, Complete=false, Length=9
  ... (160 more runs)
  Summary: 10 pieces with Priority=None, 662 pieces with Priority>0  ← PIECES HAVE PRIORITY

File Info:
  File Priority: 1
  Piece priorities: 0 with None, 672 with Normal  ← Pieces get priority!

Pieces completing:
  [PIECE_COMPLETE:74991141:279]
  [PIECE_COMPLETE:74991141:365]
  [PIECE_COMPLETE:74991141:334]

Result: 4.68% complete (8.2 MB), pieces downloading and completing
```

### Root Cause Analysis

**The Breaking Change:** Upstream commit **5aa05565** (May 21, 2025) by Matt Joiner:
> "Add IgnoreUnverifiedPieceCompletion and move fields to AddTorrentOpts"

This was merged into our fork via commit **edb0a458** (Oct 6, 2025).

#### What Changed in the Library

**OLD CODE (working):**
```go
// torrent.go - old version
func (t *Torrent) pieceCompleteUncached(piece pieceIndex) storage.Completion {
    if t.storage == nil {
        return storage.Completion{Complete: false, Ok: true}  // Ok=true!
    }
    return t.pieces[piece].Storage().Completion()
}

func (t *Torrent) updatePieceCompletion(piece pieceIndex) bool {
    p := t.piece(piece)
    uncached := t.pieceCompleteUncached(piece)
    cached := p.completion()
    changed := cached != uncached
    p.storageCompletionOk = uncached.Ok  // Always sets from storage
    // ... rest
}

// Called from onSetInfo:
t.updatePieceCompletion(i)  // Sets storageCompletionOk immediately
t.queueInitialPieceCheck(i)
```

**NEW CODE (broken):**
```go
// torrent.go - new version
func (t *Torrent) pieceCompleteUncached(piece pieceIndex) (ret storage.Completion) {
    p := t.piece(piece)
    if t.ignoreUnverifiedPieceCompletion && p.numVerifies == 0 {
        return  // Returns Ok: false, breaking everything!
    }
    if t.storage == nil {
        return storage.Completion{Complete: false, Ok: false}  // Ok=false!
    }
    return p.Storage().Completion()
}

func (t *Torrent) setInitialPieceCompletionFromStorage(piece pieceIndex) {
    t.setCachedPieceCompletionFromStorage(piece)
    t.afterSetPieceCompletion(piece, true)
}

func (t *Torrent) queueInitialPieceCheck(i pieceIndex) {
    if t.initialPieceCheckDisabled {
        return  // Returns early, piece check never happens
    }
    p := t.piece(i)
    if p.numVerifies != 0 {
        return
    }
    if p.storageCompletionOk {
        return  // But storageCompletionOk is false!
    }
    _, _ = t.queuePieceCheck(i)
}

// Called from onSetInfo:
t.setInitialPieceCompletionFromStorage(i)  // storageCompletionOk stays false
t.queueInitialPieceCheck(i)  // Returns early, no check queued
```

#### The Chicken-and-Egg Problem

**piece.go:ignoreForRequests()** checks multiple conditions:
```go
func (p *Piece) ignoreForRequests() bool {
    // FIRST CHECK - this is what fails
    if !p.storageCompletionOk {
        // Piece completion unknown - IGNORE THIS PIECE
        return true
    }

    if p.hashing || p.marking || !p.haveHash() || p.t.dataDownloadDisallowed.IsSet() {
        return true
    }
    if p.t.pieceComplete(p.index) {
        return true
    }
    if p.queuedForHash() {
        return true
    }
    return false
}
```

**The Sequence When DisableInitialPieceCheck=true:**

1. `onSetInfo()` calls `setInitialPieceCompletionFromStorage(i)`
2. Which calls `pieceCompleteUncached(i)`
3. Since `initialPieceCheckDisabled=true`, upstream commit 5aa05565 made it so `ignoreUnverifiedPieceCompletion` is implicitly true
4. Since `p.numVerifies == 0` (never checked), `pieceCompleteUncached` returns `Ok: false`
5. `storageCompletionOk` gets set to `false`
6. Later, `queueInitialPieceCheck(i)` returns early because `initialPieceCheckDisabled=true`
7. Piece never gets checked, `numVerifies` stays 0, `storageCompletionOk` stays false
8. When priority is set via `File.SetPriority()` or `t.DownloadAll()`, it calls `updatePiecePriority()`
9. Which calls `updatePiecePriorityNoRequests()` → `updatePieceRequestOrderPiece()`
10. Which checks `ignorePieceForRequests()` → returns true because `!storageCompletionOk`
11. Piece is NOT added to request order
12. Piece priority stays 0 (None)
13. No requests are made for this piece
14. Downloads stall completely

**The Fix Requirement:**

When `DisableInitialPieceCheck=true`, the library MUST still set `storageCompletionOk=true` by actually calling storage.Completion(), it just shouldn't QUEUE a hash check.

The problem is commit 5aa05565 conflated two concepts:
- "Don't hash verify pieces initially" (DisableInitialPieceCheck)
- "Don't trust unverified piece completion" (IgnoreUnverifiedPieceCompletion)

These should be independent, but the code treats them as related.

### Why This Worked Before

**Before commit edb0a458 (the merge):**
- `updatePieceCompletion()` ALWAYS called storage.Completion() and set `storageCompletionOk`
- Even with `DisableInitialPieceCheck=true`, pieces got `storageCompletionOk=true`
- `queueInitialPieceCheck()` would return early (no hash check), but `storageCompletionOk` was already set
- Pieces could be requested normally

**After commit edb0a458:**
- New `setInitialPieceCompletionFromStorage()` + `ignoreUnverifiedPieceCompletion` logic
- If piece never verified (`numVerifies==0`), returns `Ok: false`
- `storageCompletionOk` never gets set to true
- Pieces permanently ignored for requests

### Why DisableInitialPieceCheck Was Used

**Original Intent:** For large torrents (100k+ pieces) dropped in place from external source:
- Initial piece checks trigger massive I/O (hash verification of all pieces)
- Can take hours on multi-TB torrents
- We know the data is good (came from reliable source)
- Want to start serving immediately without verification overhead

**Use Cases:**
1. Migrating existing torrents between servers
2. Pre-seeding from verified external sources
3. Testing with known-good data
4. High-performance scenarios where verification is deferred

### Attempted Workarounds (Tested)

**1. Using Default Storage (FAILED):**
```go
// Tried replacing custom storage with library default
pc, _ := storage.NewDefaultPieceCompletionForDir(downloadPath + "/completion")
tc.Storages[downloadPath] = storage.NewMMapWithCompletion(downloadPath, pc)
// Result: Still 0 B/s, not a storage issue
```

**2. Skipping ApplyTorrentPriorities (FAILED):**
```go
// Tried not calling priority.go:ApplyTorrentPriorities()
// Result: Still 0 B/s, not a priority management issue
```

**3. Removing DataDir Config (FAILED):**
```go
// Tried commenting out: cfg.DataDir = "download"
// Result: Still 0 B/s, not a config issue
```

**4. Calling DownloadAll() Multiple Times (FAILED):**
```go
t.DownloadAll()  // Sets file priority
// ... wait 30s ...
t.DownloadAll()  // Try again
// Result: Still 672 pieces with Priority=None
```

**5. Commenting Out DisableInitialPieceCheck (WORKS!):**
```go
// torrentVars.Spec.DisableInitialPieceCheck = true
// Result: 9.6 MB downloaded, 4.68% complete, pieces completing!
```

### Diagnostic Evidence

**Piece Tracking Loop:**
```
[DIAGNOSTIC] Starting piece tracking loop for 74991141, totalPiecesRemaining=672
# WITH DisableInitialPieceCheck: ZERO piece state change events
# WITHOUT DisableInitialPieceCheck: Hundreds of events like:
[DIAGNOSTIC] Piece state change received: 74991141:279, Complete=true
[PIECE_COMPLETE:74991141:279]
```

**Piece State Check:**
```
# WITH DisableInitialPieceCheck:
[DIAGNOSTIC] Piece 0 state: {{false true <nil>} 1 false false false false false false}
# Priority=1 but piece never becomes available for requests

# Analysis of piece.go:ignoreForRequests() return true because:
# - !p.storageCompletionOk = true (fails first check)
```

### Solution Options

**Option 1: Fix in Torrent Library (PROPER FIX):**

Modify `torrent.go:pieceCompleteUncached()` to NOT return `Ok: false` during initialization when `DisableInitialPieceCheck=true`:

```go
func (t *Torrent) pieceCompleteUncached(piece pieceIndex) (ret storage.Completion) {
    p := t.piece(piece)

    // REMOVED THIS - was breaking DisableInitialPieceCheck:
    // if t.ignoreUnverifiedPieceCompletion && p.numVerifies == 0 {
    //     return  // Returns Ok: false
    // }

    if t.storage == nil {
        return storage.Completion{Complete: false, Ok: false}
    }
    return p.Storage().Completion()  // Always check storage
}
```

**Rationale:**
- `DisableInitialPieceCheck` should only skip the hash verification step
- Storage completion should ALWAYS be checked to set `storageCompletionOk`
- The `ignoreUnverifiedPieceCompletion` check in `pieceCompleteUncached` is wrong
- If we want to ignore unverified pieces, do it in `ignoreForRequests()` after checking `numVerifies`

**Files to change:**
- `torrent.go:328-335` (pieceCompleteUncached function)

**Option 2: Workaround in seedr_subnode (TEMPORARY):**

Remove `DisableInitialPieceCheck` from add.go, accept the initial piece check overhead:

```go
// src/download/torrentclient/add.go
// Comment out these 3 lines:
// Line 38:  torrentVars.Spec.DisableInitialPieceCheck = true
// Line 100: torrentVars.Spec.DisableInitialPieceCheck = true
// Line 149: torrentVars.Spec.DisableInitialPieceCheck = true
```

**Drawbacks:**
- Large torrents will trigger initial hash verification
- Can take hours for multi-TB torrents
- Increases disk I/O and startup time
- Defeats the purpose of pre-seeded data

**Option 3: Set Both Flags Explicitly (UNTESTED):**

Try setting `IgnoreUnverifiedPieceCompletion=false` explicitly to override the implicit behavior:

```go
torrentVars.Spec.DisableInitialPieceCheck = true
torrentVars.Spec.IgnoreUnverifiedPieceCompletion = false  // Force storage check
```

This MIGHT work if the two flags are actually independent, but the library code suggests they're not properly separated.

### Recommended Fix Path

**Immediate (to unblock):**
1. Comment out `DisableInitialPieceCheck = true` in add.go (Option 2)
2. Accept initial piece check overhead for now
3. Test and deploy

**Proper Fix (next):**
1. Fix `pieceCompleteUncached()` in torrent library (Option 1)
2. The function should ALWAYS call storage.Completion() regardless of `numVerifies`
3. The `ignoreUnverifiedPieceCompletion` logic belongs in `ignoreForRequests()`, not in `pieceCompleteUncached()`
4. Test with DisableInitialPieceCheck=true
5. Deploy library fix
6. Re-enable DisableInitialPieceCheck in add.go

### Test Commands

**Test WITH DisableInitialPieceCheck=true (broken):**
```bash
cd /home/daniel/work/main/seedr_subnode
# Ensure add.go has: torrentVars.Spec.DisableInitialPieceCheck = true
go run src/download/test/torrent.go "magnet:?xt=urn:btih:74991141FB1F52A040A76F6A5A6515D8B0B05A5B"
# Expect: 0 B/s, 0% complete, 672 pieces with Priority=None
```

**Test WITHOUT DisableInitialPieceCheck (working):**
```bash
cd /home/daniel/work/main/seedr_subnode
# Comment out: // torrentVars.Spec.DisableInitialPieceCheck = true
go run src/download/test/torrent.go "magnet:?xt=urn:btih:74991141FB1F52A040A76F6A5A6515D8B0B05A5B"
# Expect: ~10 MB/s, 4-5% in 30s, pieces completing, 662 pieces with Priority>0
```

**Test Magnet:**
```
magnet:?xt=urn:btih:74991141FB1F52A040A76F6A5A6515D8B0B05A5B
Bob's Burgers S16E02 (176 MB)
```

### Current State (Oct 7, 2025 16:00)

**Torrent Library:**
- File: `~/work/torrent/torrent.go`
- Issue: `pieceCompleteUncached()` returns `Ok: false` when `numVerifies==0`
- Needs: Remove `ignoreUnverifiedPieceCompletion` check or move to different function
- Status: ❌ NOT FIXED

**seedr_subnode:**
- File: `~/work/main/seedr_subnode/src/download/torrentclient/add.go`
- Issue: Sets `DisableInitialPieceCheck=true` which breaks with new library
- Temporary fix: Comment out the flag (3 locations)
- Status: ⚠️ WORKAROUND AVAILABLE (comments added, not removed flag yet)

### Files Modified for Testing

**add.go Changes:**
```
Line 38:  // TEMPORARY TEST: Comment out DisableInitialPieceCheck to see if that's the issue
Line 39:  // torrentVars.Spec.DisableInitialPieceCheck = true

Line 99:  // TEMPORARY TEST: Comment out DisableInitialPieceCheck to see if that's the issue
Line 100: // torrentVars.Spec.DisableInitialPieceCheck = true

Line 148: // TEMPORARY TEST: Comment out DisableInitialPieceCheck to see if that's the issue
Line 149: // torrentVars.Spec.DisableInitialPieceCheck = true
```

**new.go Changes:**
```
Line 148: cfg.DataDir = "download"  # Reverted from commented out
```

**progress.go Changes:**
```
Line 404: fmt.Printf("[DIAGNOSTIC] Starting piece tracking loop...")  # Added for debugging
Line 417: fmt.Printf("[DIAGNOSTIC] Piece state change received...")  # Added for debugging
```

**add.go Changes:**
```
Line 45-50: // DIAGNOSTIC: Check if storage is set  # Added for debugging
```

### Related Commits

**In torrent library:**
- `5aa05565` - Add IgnoreUnverifiedPieceCompletion (May 21, 2025) - THE BREAKING CHANGE
- `edb0a458` - Merge upstream integration (Oct 6, 2025) - Brought the breaking change into fork
- `d9d90257` - Overlay fork tuning (reverted some changes but not the breaking one)

**In seedr_subnode:**
- No commits yet - testing phase

### Next Conversation Should Start With

"Continue from HANDOFF.md - need to fix DisableInitialPieceCheck regression. The issue is in torrent library `pieceCompleteUncached()` returning `Ok: false` when `numVerifies==0`. Current workaround is commenting out DisableInitialPieceCheck, but proper fix is needed in the library."

**Key Questions to Answer:**
1. Should we remove the `ignoreUnverifiedPieceCompletion` check entirely from `pieceCompleteUncached()`?
2. Or move it to a different location (like `ignoreForRequests()`)?
3. What's the correct behavior when BOTH flags are set?
4. How to handle the case where storage says "complete" but piece is unverified?
5. Is upstream anacrolix/torrent affected by this, or only our fork?

**Previous Investigation Notes (for fillWriteBuffer fix - kept for reference):**

1. **tickleWriter() not actually waking writer properly**
   - Maybe writer goroutine is blocked on something else
   - Check if writeCond.Broadcast() is working

2. **needRequestUpdate flag being cleared before writer sees it**
   - Race condition between set and read?
   - Check maybeUpdateActualRequestState() timing

3. **Writer waking but not processing updates**
   - Maybe fillWriteBuffer() returns early?
   - Check writeBuffer.Len() > writeBufferLowWaterLen condition

4. **Different code path entirely**
   - Maybe not File.SetPriority but something else?
   - Need to trace actual download initiation

5. **allowDefers=false in more places than we found**
   - Grep for all internal.Lock usage
   - Check if any call onNeedUpdateRequests

**Next Steps to Debug:**

1. **Check writer goroutine status on live stalled server:**
   ```bash
   # Get goroutine dump
   ssh root@SERVER 'curl -s "http://localhost:6060/debug/pprof/goroutine?debug=2"' > /tmp/stuck.txt

   # Look for writer goroutines
   grep -A 20 "messageWriterRunner\|peerConnMsgWriter.run" /tmp/stuck.txt

   # Look for fillWriteBuffer calls
   grep -B 5 -A 15 "fillWriteBuffer" /tmp/stuck.txt

   # Look for maybeUpdateActualRequestState
   grep -B 5 -A 10 "maybeUpdateActualRequestState" /tmp/stuck.txt
   ```

2. **Add diagnostic logging to peerconn.go:**
   ```go
   // In onNeedUpdateRequests around line 1814
   if !cn.locker().allowDefers {
       cn.t.logger.Printf("DEBUG: onNeedUpdateRequests with allowDefers=false, reason=%s, calling tickleWriter", reason)
       cn.tickleWriter()
   }

   // In maybeUpdateActualRequestState around line 269
   func (p *PeerConn) maybeUpdateActualRequestState() {
       p.t.logger.Printf("DEBUG: maybeUpdateActualRequestState called, needRequestUpdate=%s", p.needRequestUpdate)
       if p.needRequestUpdate == "" {
           return
       }
       // ... rest of function
   }
   ```

3. **Check if downloads are even starting:**
   ```bash
   # Look for initial connection/handshake
   grep -i "handshook\|connection.*established" logs/

   # Check if pieces are being requested at all
   grep -i "request\|piece" logs/ | head -100

   # Check peer state
   curl localhost:6060/debug/pprof/goroutine | grep -c "runHandshookConn"
   ```

4. **Verify the actual code path:**
   ```bash
   # Check all internal.Lock usage
   cd ~/work/torrent
   grep -n "internal.Lock" *.go

   # For each, check if it leads to onNeedUpdateRequests
   grep -A 30 "internal.Lock" file.go client.go
   ```

5. **Check if issue is specific to certain operations:**
   - Does it happen on fresh torrent add? Or only after SetPriority?
   - Does it happen with all torrents or only large ones?
   - Does it happen immediately or after some time?

6. **Alternative hypothesis - check if requests ARE being made but not sent:**
   ```bash
   # Check if requestState has pending requests
   # Look for requestState.Requests in goroutine dump
   grep "requestState" /tmp/stuck.txt

   # Check if choked by peer
   grep "peerChoking\|Interested" /tmp/stuck.txt
   ```

**Test Magnet:** (provided by user)
```
magnet:?xt=urn:btih:AF728E48EF45DDFE823FFC545AB32670BA4DFB59&dn=Elio.2025.1080p...
```

## Testing & Deployment

### Affected Servers
- **rd6**: Initial deadlock discovery (438 goroutines blocked)
- **nw4**: Performance bottleneck (559 goroutines blocked on CPU work)
- **nw23**: Webseed panic
- **rd25**: Option panic

### Test Magnet
User provided this magnet to test downloads:
```
magnet:?xt=urn:btih:AF728E48EF45DDFE823FFC545AB32670BA4DFB59&dn=Elio.2025.1080p.ITA-ENG.BluRay.x265.AAC-V3SP4EV3R.mkv&tr=...
```

### Diagnostic Tools Used
```bash
# Lock holders (who has the lock)
ssh root@SERVER "curl -s localhost:6060/debug/pprof/lockHolders" > /tmp/lockholders.pprof
go tool pprof -text /tmp/lockholders.pprof

# Lock blockers (who's waiting)
ssh root@SERVER "curl -s localhost:6060/debug/pprof/lockBlockers" > /tmp/lockblockers.pprof
go tool pprof -text /tmp/lockblockers.pprof

# Full goroutine dump
ssh root@SERVER 'curl -s "http://localhost:6060/debug/pprof/goroutine?debug=2"' > /tmp/goroutines.txt
```

### go.mod Version
Current: `v0.0.0-20251007052638-aa84070e0721`

## Known Patterns to Avoid

### ❌ DON'T: Use internal lock if code path calls onNeedUpdateRequests
```go
// BAD - will cause stalls or panics
cl._mu.internal.Lock()
doSomethingThatCallsOnNeedUpdateRequests()
cl._mu.internal.Unlock()
```

### ✅ DO: Use regular lock if defers might be needed
```go
// GOOD
cl.lock()
doSomethingThatCallsOnNeedUpdateRequests()
cl.unlock()
```

### ❌ DON'T: Try to lock ws.locker when Client lock is already held
```go
// BAD - ws.locker IS the Client lock!
cl.lock()
ws.locker.Lock() // DEADLOCK
```

### ✅ DO: Check if lock is already held before calling sub-functions
```go
// GOOD - document that caller must hold lock
// NOTE: Caller must hold Client lock
func (ws *webseedPeer) someFunction() {
    // No lock acquisition - caller already has it
    ws.requestQueue = nil
}
```

## If Downloads Stall Again

### Check These First:
1. Is `needRequestUpdate` being set but writer not waking?
   - Look for `onNeedUpdateRequests` calls with `allowDefers=false`
   - Ensure `tickleWriter()` or defer is called

2. Are deferred actions running?
   - Check if code uses `internal.Lock` where it should use regular `lock`
   - Verify `Unlock()` runs deferred actions (not `internal.Unlock()`)

3. Is writer blocked on lock?
   - Check `fillWriteBuffer()` calls in goroutine dump
   - Look for lock holder in pprof

### Check These for Deadlocks:
1. Recursive lock acquisition
   - Same goroutine tries to acquire lock twice
   - Common: `ws.locker` when Drop/close already holds Client lock

2. Lock-then-channel vs channel-then-lock
   - Goroutine A: holds lock, sends on channel
   - Goroutine B: waiting on channel, wants lock
   - **Classic deadlock**

3. Deferred action trying to acquire lock
   - Unlock runs deferred actions while transitioning
   - Deferred action tries to acquire same lock
   - Use internal lock to avoid if this is the issue

## Future Upstream Merges

### Before Merging:
1. Review commits for lock changes:
   ```bash
   git log --all --grep="lock\|Lock\|defer" upstream/master ^origin/master
   ```

2. Check for new `internal.Lock` usage:
   ```bash
   git diff upstream/master | grep "internal.Lock"
   ```

3. Look for calls to `onNeedUpdateRequests`:
   ```bash
   git diff upstream/master | grep -B 5 "onNeedUpdateRequests"
   ```

### After Merging:
1. Test with large torrents (100k+ pieces) for performance issues
2. Enable debug mode to trigger validation checks
3. Test file priority changes
4. Test webseed torrents
5. Monitor lockHolders/lockBlockers metrics

## Additional Notes

### Webseed Concurrency
- Each webseed peer spawns up to `MaxRequests` requester goroutines
- Default `MaxRequests` = 2
- All share same `ws.locker` (which is Client lock)
- Concurrent spawns can create duplicate sliceIndex requests

### Request Update Flow
```
User action (SetPriority, piece complete, etc.)
  → onNeedUpdateRequests(reason)
    → if allowDefers:
        defer handleOnNeedUpdateRequests
      else:
        tickleWriter()

Writer goroutine (separate thread):
  → wakes from Broadcast
  → fillWriteBuffer()
    → maybeUpdateActualRequestState()
      → if needRequestUpdate != "":
          updateRequests()
```

### Debug Flags
- `ClientConfig.Debug` - enables expensive validation
- `webseed.PrintDebug` - enables webseed debug output
- `debugMetricsEnabled` - controls metric collection

## Contact
- Original debugging session: October 6-7, 2025
- All fixes authored by: daniel <danielzeev@gmail.com> + Claude Code
- Fork repo: github.com/DannyZB/torrent
- Upstream: github.com/anacrolix/torrent
