# WebSeed Request Mechanism Analysis

## Summary

After analyzing the code, I've found that webseed peers DO use the standard `requestState.Requests` bitmap, and this appears to be the intended design. Here's how it works:

## How WebSeed Requests Work

### 1. `handleUpdateRequests` Implementation for WebSeed
```go
func (ws *webseedPeer) handleUpdateRequests() {
    // Because this is synchronous, webseed peers seem to get first dibs on newly prioritized
    // pieces.
    ws.peer.maybeUpdateActualRequestState()
}
```
This is a simple wrapper that calls the standard peer request update mechanism.

### 2. `maybeUpdateActualRequestState` Function
```go
func (p *Peer) maybeUpdateActualRequestState() {
    if p.closed.IsSet() {
        return
    }
    if p.needRequestUpdate == "" {
        return
    }
    // ... timing checks ...
    next := p.getDesiredRequestState()
    p.applyRequestState(next)
    p.t.cacheNextRequestIndexesForReuse(next.Requests.requestIndexes)
}
```
This is the standard mechanism used by ALL peers (including webseed).

### 3. `applyRequestState` Function
This function adds requests to the `requestState.Requests` bitmap by calling:
```go
more = p.mustRequest(req)
```

Which in turn calls:
```go
func (cn *Peer) request(r RequestIndex) (more bool, err error) {
    // ... validation checks ...
    cn.requestState.Requests.Add(r)  // <-- Adds to the bitmap
    // ... update tracking ...
    return cn.legacyPeerImpl._request(ppReq), nil
}
```

### 4. WebSeed's `_request` Implementation
```go
func (ws *webseedPeer) _request(r Request) bool {
    select {
    case ws.requesterWakeup <- struct{}{}:
    default:
        // Channel full, requesters will wake up anyway
    }
    return true
}
```
Instead of sending a request message over the wire (like PeerConn does), webseed just signals its requester goroutines to wake up.

### 5. WebSeed's Requester Goroutines
```go
func (ws *webseedPeer) requester(i int) {
    for !ws.peer.closed.IsSet() {
        ws.peer.t.cl._mu.internal.Lock()
        processedAnyRequests := false
        for reqIndex := range ws.peer.requestState.Requests.Iterator() {  // <-- Reads from the bitmap
            if !ws.requestIteratorLocked(i, reqIndex) {
                // ... error handling ...
            }
            processedAnyRequests = true
        }
        // ... wait for more work ...
    }
}
```

## Key Differences Between WebSeed and Regular Peers

1. **Regular Peers (PeerConn)**:
   - `_request()` sends a request message over the wire
   - Waits for pieces to come back asynchronously
   - One request = one network message

2. **WebSeed Peers**:
   - `_request()` just wakes up requester goroutines
   - Multiple requester goroutines (default 16) poll the `requestState.Requests` bitmap
   - Each requester handles one request at a time from the bitmap
   - Uses HTTP range requests to fetch data

## Why WebSeed Uses the Request State Bitmap

The bitmap is necessary because:

1. **Request Tracking**: The torrent needs to know which pieces are being requested from which peers
2. **Coordination**: Prevents duplicate requests across different peers
3. **Cancellation**: Allows the torrent to cancel requests when needed
4. **State Management**: Tracks what's in-flight vs completed

## Conclusion

WebSeed peers SHOULD have requests in their `requestState.Requests` bitmap. This is the correct behavior and is how the system tracks what pieces are being downloaded from webseed sources. The difference is not in whether they use the bitmap, but in HOW they process those requests (via HTTP range requests in goroutines vs BitTorrent protocol messages).

The bitmap serves as a queue that the webseed requester goroutines consume from, which is different from regular peers where the bitmap tracks messages that have already been sent over the wire.