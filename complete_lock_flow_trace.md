# Complete Lock Flow Trace for WebseedPeer

## 1. Creation and Initialization Flow

### Entry Point: torrent.go:3138-3174
```go
ws := webseedPeer{...}
// ...initialization...
for i := 0; i < ws.client.MaxRequests; i += 1 {
    go ws.requester(i)  // Start multiple goroutines
}
```
- **Lock State**: No locks held
- **Creates**: Multiple requester goroutines (typically 16)

## 2. Main Request Processing Loop

### requester() function (webseed-peer.go:131-165)

#### Loop Start (line 133)
```go
for !ws.peer.closed.IsSet() {
    ws.peer.t.cl._mu.internal.Lock()  // ACQUIRES internal lock
```
- **Lock State**: Internal lock acquired

#### Request Processing (lines 137-146)
```go
for reqIndex := range ws.peer.requestState.Requests.Iterator() {
    if !ws.requestIteratorLocked(i, reqIndex) {
        ws.peer.t.cl._mu.internal.Unlock()  // RELEASES internal lock
        goto start
    }
    processedAnyRequests = true
}
```
- **Lock State**: Internal lock held throughout iteration
- **Calls**: `requestIteratorLocked` with lock held

#### No Requests Path (lines 148-164)
```go
if processedAnyRequests {
    ws.peer.t.cl._mu.internal.Unlock()  // RELEASES internal lock
    continue
}
ws.peer.t.cl._mu.internal.Unlock()  // RELEASES internal lock
select {
    case <-ws.requesterWakeup:
    case <-ws.requesterClosed:
    case <-ws.peer.closed.Done():
}
```
- **Lock State**: Lock released before waiting

## 3. Request Iterator Function

### requestIteratorLocked() (webseed-peer.go:90-129)

#### Entry
- **Precondition**: Internal lock MUST be held by caller
- **Lock State**: Internal lock held

#### Path 1: Request Already Active (lines 101-103)
```go
if _, ok := ws.activeRequests[r]; ok {
    return true  // Returns with lock still held
}
```
- **Lock State**: Returns with internal lock held

#### Path 2: Process New Request (lines 104-128)
```go
webseedRequest := ws.client.StartNewRequest(ws.intoSpec(r))
ws.activeRequests[r] = webseedRequest
ws.peer.t.cl._mu.internal.Unlock()  // RELEASES internal lock

err = ws.requestResultHandler(r, webseedRequest)  // Network I/O

ws.peer.t.cl._mu.internal.Lock()    // RE-ACQUIRES internal lock
delete(ws.activeRequests, r)
```

##### Path 2a: Error Case (lines 113-125)
```go
if err != nil {
    ws.peer.t.cl._mu.internal.Unlock()  // RELEASES internal lock
    select {
        case <-time.After(...):          // Sleep without lock
    }
    ws.peer.t.cl._mu.internal.Lock()    // RE-ACQUIRES internal lock
    return false  // Returns with lock held
}
```

##### Path 2b: Success Case (lines 126-127)
```go
return true  // Returns with lock held
```

## 4. Request Result Handler

### requestResultHandler() (webseed-peer.go:202-250)
- **Entry**: Called WITHOUT lock held
- **Lock Operations**:
  ```go
  ws.peer.t.cl._mu.internal.Lock()    // Line 213 - ACQUIRES internal lock
  defer ws.peer.t.cl._mu.internal.Unlock()  // Line 214 - Deferred RELEASE
  ```
- **Exit**: Returns without lock (deferred unlock)

## 5. Other Callers and Entry Points

### _request() (webseed-peer.go:79-86)
```go
func (ws *webseedPeer) _request(r Request) bool {
    select {
    case ws.requesterWakeup <- struct{}{}:  // Signal requester
    default:
    }
    return true
}
```
- **Lock Expectation**: Should be called with lock held
- **Called From**: `Peer.request()` → `Peer.mustRequest()` → called from `applyRequestState()`

### handleUpdateRequests() (webseed-peer.go:178-182)
```go
func (ws *webseedPeer) handleUpdateRequests() {
    ws.peer.maybeUpdateActualRequestState()
}
```
- **Lock Expectation**: Should be called with lock held
- **Called From**: `Peer.updateRequests()` which is called from various places

## 6. Complete Flow Example

1. **Initial State**: No locks held
2. **requester goroutine starts**: Acquires internal lock
3. **Finds request**: Calls `requestIteratorLocked` with lock held
4. **requestIteratorLocked**: 
   - Releases lock for network I/O
   - Calls `requestResultHandler` (no lock)
   - `requestResultHandler` acquires/releases its own lock
   - Re-acquires lock after I/O
5. **Returns to requester**: Lock still held
6. **requester continues**: Either processes more or releases lock to wait

## 7. Identified Issues

### Issue 1: Lock Type Mismatch (CRITICAL)
- **Problem**: Inconsistent lock types between caller and callee
- **Details**:
  - `requester()` uses `_mu.internal.Lock/Unlock`
  - `requestIteratorLocked()` uses `_mu.internal.Lock/Unlock` (CORRECT as of current code)
  - Both are now consistent

### Issue 2: Panic Recovery
- **Current**: Panic recovery in `requestIteratorLocked` maintains lock invariant
- **Risk**: If panic occurs after unlock but before re-lock, state could be inconsistent

## 8. Edge Cases and Error Paths

### Edge Case 1: Peer Closure During Request
- Handled by checking `ws.peer.closed.IsSet()` at multiple points
- Lock properly released in all paths

### Edge Case 2: Torrent Closure During Request
- Checked in `requestResultHandler` after acquiring lock
- Returns nil (no error) if torrent is closed

### Edge Case 3: Request Cancellation
- Handled in `_cancel()` method
- Properly removes from activeRequests map

## 9. Verification Summary

✅ **Lock Invariants Maintained**: All paths maintain the invariant that if entered with lock, exit with lock
✅ **No Deadlocks**: Lock is properly released during I/O operations
✅ **Consistent Lock Types**: Both functions use internal locks
✅ **Proper Error Handling**: All error paths maintain lock consistency
✅ **Safe Concurrent Access**: activeRequests map protected by lock

## 10. Recommendations

1. **Current code appears correct** - The lock flow is consistent and safe
2. **Consider adding lock assertions** for debugging:
   ```go
   // At function entry
   if !ws.peer.t.cl._mu.internal.TryLock() {
       // Lock is held as expected
       ws.peer.t.cl._mu.internal.Unlock()
   } else {
       panic("lock not held at entry")
   }
   ```
3. **Document lock requirements** in function comments
4. **Consider using lock annotations** if available in your toolchain