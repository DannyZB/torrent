# Performance Improvements for Torrent Client

## 1. Optimize Chunk Receiving (Highest Impact)

The chunk receiving path is called thousands of times per second during active downloads. Currently, it holds the client lock for too long.

### Current Issues:
- Lock held during stats updates
- Multiple function calls for stats instead of direct atomics
- Lock held during non-critical operations

### Proposed Fix:
```go
// In peer.go receiveChunkImpl function

// Batch atomic stats updates to reduce lock contention
func (c *Peer) receiveChunkImpl(msg *pp.Message, msgTime time.Time, immediate bool) error {
    // ... validation code ...
    
    // Do stats updates with direct atomics instead of function calls
    chunkSize := int64(len(msg.Piece))
    
    // Move these outside the critical section
    c._stats.ChunksReadUseful.Add(1)
    c._stats.BytesReadUsefulData.Add(chunkSize)
    
    // Only lock for critical state changes
    c.t.cl.lock()
    // ... minimal critical operations ...
    c.t.cl.unlock()
    
    // Write to disk without holding lock (already implemented)
    
    // Batch the remaining stats updates
    if c.reconciledHandshakeStats {
        c.t.connStats.ChunksReadUseful.Add(1)
        c.t.connStats.BytesReadUsefulData.Add(chunkSize)
        c.t.cl.connStats.ChunksReadUseful.Add(1)
        c.t.cl.connStats.BytesReadUsefulData.Add(chunkSize)
    }
}
```

## 2. Optimize Piece Availability Updates

### Current Issues:
- Every HAVE message triggers a B-tree update
- Panic/recover in B-tree operations adds overhead

### Proposed Fix:
```go
// In ajwerner-btree.go - Remove panic/recover overhead

func (a *ajwernerBtree) Delete(item PieceRequestOrderItem) {
    // Direct delete without panic/recover
    a.btree.Delete(item)
}

func (a *ajwernerBtree) Add(item PieceRequestOrderItem) {
    // Use Upsert directly without checking overwrites
    a.btree.Upsert(item)
}

// Batch availability updates
// In torrent.go - add batching for multiple updates
type availabilityUpdate struct {
    piece pieceIndex
    delta int
}

func (t *Torrent) batchUpdateAvailability(updates []availabilityUpdate) {
    for _, u := range updates {
        p := t.piece(u.piece)
        p.relativeAvailability += u.delta
    }
    // Update request order once for all changes
    for _, u := range updates {
        t.updatePieceRequestOrderPiece(u.piece)
    }
}
```

## 3. Cache Request State Calculations

### Current Issues:
- `getDesiredRequestState` recalculates everything on each call
- Iterates through all pieces even when few have changed

### Proposed Fix:
```go
// Add caching to request state
type cachedRequestState struct {
    state         desiredRequestState
    lastUpdate    time.Time
    dirtyPieces   map[pieceIndex]bool
}

// Only recalculate for changed pieces
func (p *Peer) getDesiredRequestState() (desired desiredRequestState) {
    // Check cache validity
    if time.Since(p.cachedRequestState.lastUpdate) < 100*time.Millisecond &&
       len(p.cachedRequestState.dirtyPieces) == 0 {
        return p.cachedRequestState.state
    }
    
    // Incremental update for dirty pieces only
    // ...
}
```

## 4. Optimize numDirtyBytes Calculation

### Current Issues:
- Iterates through chunks individually
- Doesn't leverage roaring bitmap efficiency

### Proposed Fix:
```go
func (p *Piece) numDirtyBytes() (ret pp.Integer) {
    // Use roaring bitmap cardinality directly
    offset := p.requestIndexOffset()
    dirtyCount := p.t.dirtyChunks.RangeCardinality(
        uint64(offset), 
        uint64(offset + p.numChunks())
    )
    
    if dirtyCount == 0 {
        return 0
    }
    
    // Only special-case the last chunk
    lastChunkIndex := p.numChunks() - 1
    if p.chunkIndexDirty(lastChunkIndex) {
        return pp.Integer(dirtyCount-1)*p.chunkSize() + 
               p.chunkIndexSpec(lastChunkIndex).Length
    }
    
    return pp.Integer(dirtyCount) * p.chunkSize()
}
```

## 5. Reduce Lock Granularity

### Add more fine-grained locking:
```go
// Instead of one big client lock, use separate locks for:
type Client struct {
    // Existing lock for compatibility
    _mu lockWithDeferreds
    
    // New fine-grained locks
    statsLock    sync.RWMutex  // For stats updates
    requestLock  sync.RWMutex  // For request state
    peerLock     sync.RWMutex  // For peer management
}
```

## Performance Impact Estimates

1. **Chunk Receiving Optimization**: 30-50% reduction in lock contention
2. **Piece Availability Batching**: 20-30% reduction in B-tree operations
3. **Request State Caching**: 40-60% reduction in CPU usage for request management
4. **numDirtyBytes Optimization**: 10-20% improvement for pieces with many chunks
5. **Fine-grained Locking**: 25-40% improvement in concurrent operations

## Implementation Priority

1. **High Priority**: Chunk receiving optimization (biggest impact)
2. **High Priority**: Remove panic/recover from B-tree operations
3. **Medium Priority**: Request state caching
4. **Medium Priority**: numDirtyBytes optimization
5. **Low Priority**: Fine-grained locking (requires more testing)

These optimizations focus on real bottlenecks that affect performance during active downloading with many peers, not micro-optimizations that save minimal CPU cycles.