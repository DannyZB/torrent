package torrent

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/anacrolix/log"

	"github.com/anacrolix/torrent/metainfo"
	pp "github.com/anacrolix/torrent/peer_protocol"
	"github.com/anacrolix/torrent/webseed"
)

const (
	webseedPeerCloseOnUnhandledError = false
)

type webseedPeer struct {
	// First field for stats alignment.
	peer             Peer
	client           webseed.Client
	activeRequests   map[Request]webseed.Request
	// Channel-based condition variable to avoid lockWithDeferreds incompatibility
	requesterWakeup  chan struct{}
	requesterClosed  chan struct{}
	lastUnhandledErr time.Time
}

func (me *webseedPeer) lastWriteUploadRate() float64 {
	// We never upload to webseeds.
	return 0
}

var _ legacyPeerImpl = (*webseedPeer)(nil)

func (me *webseedPeer) peerImplStatusLines() []string {
	return []string{
		me.client.Url,
		fmt.Sprintf("last unhandled error: %v", eventAgeString(me.lastUnhandledErr)),
	}
}

func (ws *webseedPeer) String() string {
	return fmt.Sprintf("webseed peer for %q", ws.client.Url)
}

func (ws *webseedPeer) onGotInfo(info *metainfo.Info) {
	ws.client.SetInfo(info)
	// There should be probably be a callback in Client instead, so it can remove pieces at its whim
	// too.
	ws.client.Pieces.Iterate(func(x uint32) bool {
		ws.peer.t.incPieceAvailability(pieceIndex(x))
		return true
	})
}

func (ws *webseedPeer) writeInterested(interested bool) bool {
	return true
}

func (ws *webseedPeer) _cancel(r RequestIndex) bool {
	if active, ok := ws.activeRequests[ws.peer.t.requestIndexToRequest(r)]; ok {
		active.Cancel()
		// The requester is running and will handle the result.
		return true
	}
	// There should be no requester handling this, so no further events will occur.
	return false
}

func (ws *webseedPeer) intoSpec(r Request) webseed.RequestSpec {
	return webseed.RequestSpec{
		Start:  ws.peer.t.requestOffset(r),
		Length: int64(r.Length),
	}
}

func (ws *webseedPeer) _request(r Request) bool {
	select {
	case ws.requesterWakeup <- struct{}{}:
	default:
		// Channel full, requesters will wake up anyway
	}
	return true
}

// Returns true if we should look for another request to start. Returns false if we handled this
// one.
func (ws *webseedPeer) requestIteratorLocked(requesterIndex int, x RequestIndex) bool {
	r := ws.peer.t.requestIndexToRequest(x)
	if _, ok := ws.activeRequests[r]; ok {
		return true
	}
	webseedRequest := ws.client.StartNewRequest(ws.intoSpec(r))
	ws.activeRequests[r] = webseedRequest
	// Release client lock during network request  
	ws.peer.t.cl._mu.internal.Unlock()
	
	err := ws.requestResultHandler(r, webseedRequest)
	
	ws.peer.t.cl._mu.internal.Lock()
	delete(ws.activeRequests, r)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			ws.peer.logger.Levelf(log.Debug, "requester %v: error doing webseed request %v: %v", requesterIndex, r, err)
		}
		// Handle error with backoff, release lock during sleep
		ws.peer.t.cl._mu.internal.Unlock()
		select {
		case <-ws.peer.closed.Done():
		case <-time.After(time.Duration(rand.Int63n(int64(10 * time.Second)))):
		}
		ws.peer.t.cl._mu.internal.Lock() // Re-acquire lock before returning
		return false
	}
	// Success - return true with lock still held
	return true

}

func (ws *webseedPeer) requester(i int) {
start:
	for !ws.peer.closed.IsSet() {
		ws.peer.t.cl._mu.internal.Lock() // Use internal lock to bypass deferred actions
		// Check for requests while holding the client lock
		processedAnyRequests := false
		for reqIndex := range ws.peer.requestState.Requests.Iterator() {
			// requestIteratorLocked returns with lock held in all cases
			if !ws.requestIteratorLocked(i, reqIndex) {
				// Error occurred, lock is still held, unlock before restart
				ws.peer.t.cl._mu.internal.Unlock()
				goto start
			}
			// Request was processed successfully, check for more
			processedAnyRequests = true
		}
		
		if processedAnyRequests {
			// We processed requests, unlock and immediately check for more
			ws.peer.t.cl._mu.internal.Unlock() // Use internal unlock to bypass deferred actions
			continue
		}
		
		// No requests to process, unlock and wait for signal
		ws.peer.t.cl._mu.internal.Unlock() // Use internal unlock to bypass deferred actions
		select {
		case <-ws.requesterWakeup:
			// Wakeup signal received, check for more work
		case <-ws.requesterClosed:
			return
		case <-ws.peer.closed.Done():
			return
		}
	}
}

func (ws *webseedPeer) connectionFlags() string {
	return "WS"
}

// Maybe this should drop all existing connections, or something like that.
func (ws *webseedPeer) drop() {}

func (cn *webseedPeer) ban() {
	cn.peer.close()
}

func (ws *webseedPeer) handleUpdateRequests() {
	// Because this is synchronous, webseed peers seem to get first dibs on newly prioritized
	// pieces.
	ws.peer.maybeUpdateActualRequestState()
}

func (ws *webseedPeer) onClose() {
	ws.peer.logger.Levelf(log.Debug, "closing")
	// Just deleting them means we would have to manually cancel active requests.
	ws.peer.cancelAllRequests()
	ws.peer.t.iterPeers(func(p *Peer) {
		if p.isLowOnRequests() {
			p.updateRequests("webseedPeer.onClose")
		}
	})
	// Safe close: check if already closed to avoid panic
	select {
	case <-ws.requesterClosed:
		// Already closed
	default:
		close(ws.requesterClosed)
	}
}

func (ws *webseedPeer) requestResultHandler(r Request, webseedRequest webseed.Request) error {
	result := <-webseedRequest.Result
	close(webseedRequest.Result) // one-shot
	// We do this here rather than inside receiveChunk, since we want to count errors too. I'm not
	// sure if we can divine which errors indicate cancellation on our end without hitting the
	// network though.
	if len(result.Bytes) != 0 || result.Err == nil {
		// Increment ChunksRead and friends
		ws.peer.doChunkReadStats(int64(len(result.Bytes)))
	}
	ws.peer.readBytes(int64(len(result.Bytes)))
	ws.peer.t.cl._mu.internal.Lock()
	// Note: receiveChunkImpl will unlock and re-lock the mutex, so we must not defer unlock here
	lockHeld := true
	defer func() {
		if lockHeld {
			ws.peer.t.cl._mu.internal.Unlock()
		}
	}()
	if ws.peer.t.closed.IsSet() {
		return nil
	}
	err := result.Err
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
		case errors.Is(err, webseed.ErrTooFast):
		case ws.peer.closed.IsSet():
		default:
			ws.peer.logger.Printf("Request %v rejected: %v", r, result.Err)
			if webseedPeerCloseOnUnhandledError {
				log.Printf("closing %v", ws)
				ws.peer.close()
			} else {
				ws.lastUnhandledErr = time.Now()
			}
		}
		if !ws.peer.remoteRejectedRequest(ws.peer.t.requestIndexFromRequest(r)) {
			ws.peer.logger.Printf("Request %v rejected: invalid reject", r)
			return errors.New("invalid reject")
		}
		return err
	}
	// receiveChunkImpl will unlock and re-lock the mutex internally
	lockHeld = false
	err = ws.peer.receiveChunkFromWebseed(&pp.Message{
		Type:  pp.Piece,
		Index: r.Index,
		Begin: r.Begin,
		Piece: result.Bytes,
	}, time.Now())
	// After receiveChunkFromWebseed, the lock is held again
	lockHeld = true
	if err != nil {
		ws.peer.logger.Printf("error receiving chunk for request %v: %v", r, err)
		return err
	}
	return err
}

func (me *webseedPeer) peerPieces() *roaring.Bitmap {
	return &me.client.Pieces
}

func (cn *webseedPeer) peerHasAllPieces() (all, known bool) {
	if !cn.peer.t.haveInfo() {
		return true, false
	}
	return cn.client.Pieces.GetCardinality() == uint64(cn.peer.t.numPieces()), true
}
