package torrent

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/anacrolix/chansync"
	. "github.com/anacrolix/generics"
	"github.com/anacrolix/log"
	"github.com/anacrolix/missinggo/iter"
	"github.com/anacrolix/missinggo/v2/bitmap"
	"github.com/anacrolix/multiless"

	"github.com/anacrolix/torrent/internal/alloclim"
	"github.com/anacrolix/torrent/mse"
	pp "github.com/anacrolix/torrent/peer_protocol"
	request_strategy "github.com/anacrolix/torrent/request-strategy"
	typedRoaring "github.com/anacrolix/torrent/typed-roaring"
)

type (
	Peer struct {
		// First to ensure 64-bit alignment for atomics. See #262.
		_stats ConnStats

		t *Torrent

		legacyPeerImpl
		peerImpl  newHotPeerImpl
		callbacks *Callbacks

		outgoing   bool
		Network    string
		RemoteAddr PeerRemoteAddr
		// The local address as observed by the remote peer. WebRTC seems to get this right without needing hints from the
		// config.
		localPublicAddr peerLocalPublicAddr
		bannableAddr    Option[bannableAddr]
		// True if the connection is operating over MSE obfuscation.
		headerEncrypted bool
		cryptoMethod    mse.CryptoMethod
		Discovery       PeerSource
		trusted         bool
		closed          chansync.SetOnce
		// Set true after we've added our ConnStats generated during handshake to
		// other ConnStat instances as determined when the *Torrent became known.
		reconciledHandshakeStats bool

		lastMessageReceived     time.Time
		completedHandshake      time.Time
		lastUsefulChunkReceived time.Time
		lastChunkSent           time.Time

		// Stuff controlled by the local peer.
		needRequestUpdate    updateRequestReason
		requestState         request_strategy.PeerRequestState
		updateRequestsTimer  *time.Timer
		lastRequestUpdate    time.Time
		peakRequests         maxRequests
		lastBecameInterested time.Time
		priorInterest        time.Duration

		lastStartedExpectingToReceiveChunks time.Time
		cumulativeExpectedToReceiveChunks   time.Duration
		_chunksReceivedWhileExpecting       int64

		choking                                bool
		piecesReceivedSinceLastRequestUpdate   maxRequests
		maxPiecesReceivedBetweenRequestUpdates maxRequests
		// Chunks that we might reasonably expect to receive from the peer. Due to latency, buffering,
		// and implementation differences, we may receive chunks that are no longer in the set of
		// requests actually want. This could use a roaring.BSI if the memory use becomes noticeable.
		validReceiveChunks map[RequestIndex]int
		// Indexed by metadata piece, set to true if posted and pending a
		// response.
		metadataRequests []bool
		sentHaves        bitmap.Bitmap

		// Stuff controlled by the remote peer.
		peerInterested        bool
		peerChoking           bool
		peerRequests          map[Request]*peerRequestState
		PeerPrefersEncryption bool // as indicated by 'e' field in extension handshake
		// The highest possible number of pieces the torrent could have based on
		// communication with the peer. Generally only useful until we have the
		// torrent info.
		peerMinPieces pieceIndex
		// Pieces we've accepted chunks for from the peer.
		peerTouchedPieces map[pieceIndex]struct{}
		peerAllowedFast   typedRoaring.Bitmap[pieceIndex]

		PeerMaxRequests maxRequests // Maximum pending requests the peer allows.

		logger log.Logger
	}

	PeerSource string

	peerRequestState struct {
		data             []byte
		allocReservation *alloclim.Reservation
	}

	PeerRemoteAddr interface {
		String() string
	}

	peerRequests = orderedBitmap[RequestIndex]

	updateRequestReason string
)

const (
	PeerSourceUtHolepunch     = "C"
	PeerSourceTracker         = "Tr"
	PeerSourceIncoming        = "I"
	PeerSourceDhtGetPeers     = "Hg" // Peers we found by searching a DHT.
	PeerSourceDhtAnnouncePeer = "Ha" // Peers that were announced to us by a DHT.
	PeerSourcePex             = "X"
	// The peer was given directly, such as through a magnet link.
	PeerSourceDirect = "M"
)

// These are grouped because we might vary update request behaviour depending on the reason. I'm not
// sure about the fact that multiple reasons can be triggered before an update runs, and only the
// first will count. Possibly we should be signalling what behaviours are appropriate in the next
// update instead.
const (
	peerUpdateRequestsPeerCancelReason   updateRequestReason = "Peer.cancel"
	peerUpdateRequestsRemoteRejectReason updateRequestReason = "Peer.remoteRejectedRequest"
)

// Returns the Torrent a Peer belongs to. Shouldn't change for the lifetime of the Peer. May be nil
// if we are the receiving end of a connection and the handshake hasn't been received or accepted
// yet.
func (p *Peer) Torrent() *Torrent {
	return p.t
}

func (p *Peer) Stats() (ret PeerStats) {
	p.locker().RLock()
	defer p.locker().RUnlock()
	ret.ConnStats = p._stats.Copy()
	ret.DownloadRate = p.downloadRate()
	ret.LastWriteUploadRate = p.peerImpl.lastWriteUploadRate()
	ret.RemotePieceCount = p.remotePieceCount()
	return
}

func (p *Peer) initRequestState() {
	p.requestState.Requests = &peerRequests{}
}

func (cn *Peer) updateExpectingChunks() {
	if cn.expectingChunks() {
		if cn.lastStartedExpectingToReceiveChunks.IsZero() {
			cn.lastStartedExpectingToReceiveChunks = time.Now()
		}
	} else {
		if !cn.lastStartedExpectingToReceiveChunks.IsZero() {
			cn.cumulativeExpectedToReceiveChunks += time.Since(cn.lastStartedExpectingToReceiveChunks)
			cn.lastStartedExpectingToReceiveChunks = time.Time{}
		}
	}
}

func (cn *Peer) expectingChunks() bool {
	if cn.requestState.Requests.IsEmpty() {
		return false
	}
	if !cn.requestState.Interested {
		return false
	}
	if !cn.peerChoking {
		return true
	}
	haveAllowedFastRequests := false
	cn.peerAllowedFast.Iterate(func(i pieceIndex) bool {
		haveAllowedFastRequests = roaringBitmapRangeCardinality[RequestIndex](
			cn.requestState.Requests,
			cn.t.pieceRequestIndexOffset(i),
			cn.t.pieceRequestIndexOffset(i+1),
		) == 0
		return !haveAllowedFastRequests
	})
	return haveAllowedFastRequests
}

func (cn *Peer) remoteChokingPiece(piece pieceIndex) bool {
	return cn.peerChoking && !cn.peerAllowedFast.Contains(piece)
}

func (cn *Peer) cumInterest() time.Duration {
	ret := cn.priorInterest
	if cn.requestState.Interested {
		ret += time.Since(cn.lastBecameInterested)
	}
	return ret
}

func (cn *Peer) locker() *lockWithDeferreds {
	return cn.t.cl.locker()
}

func (cn *PeerConn) supportsExtension(ext pp.ExtensionName) bool {
	_, ok := cn.PeerExtensionIDs[ext]
	return ok
}

// The best guess at number of pieces in the torrent for this peer.
func (cn *Peer) bestPeerNumPieces() pieceIndex {
	if cn.t.haveInfo() {
		return cn.t.numPieces()
	}
	return cn.peerMinPieces
}

// How many pieces we think the peer has.
func (cn *Peer) remotePieceCount() pieceIndex {
	have := pieceIndex(cn.peerPieces().GetCardinality())
	if all, _ := cn.peerHasAllPieces(); all {
		have = cn.bestPeerNumPieces()
	}
	return have
}

func (cn *Peer) completedString() string {
	return fmt.Sprintf("%d/%d", cn.remotePieceCount(), cn.bestPeerNumPieces())
}

func eventAgeString(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return fmt.Sprintf("%.2fs ago", time.Since(t).Seconds())
}

// Inspired by https://github.com/transmission/transmission/wiki/Peer-Status-Text.
func (cn *Peer) statusFlags() (ret string) {
	c := func(b byte) {
		ret += string([]byte{b})
	}
	if cn.requestState.Interested {
		c('i')
	}
	if cn.choking {
		c('c')
	}
	c(':')
	ret += cn.connectionFlags()
	c(':')
	if cn.peerInterested {
		c('i')
	}
	if cn.peerChoking {
		c('c')
	}
	return
}

func (cn *Peer) downloadRate() float64 {
	num := cn._stats.BytesReadUsefulData.Int64()
	if num == 0 {
		return 0
	}
	return float64(num) / cn.totalExpectingTime().Seconds()
}

// Deprecated: Use Peer.Stats.
func (p *Peer) DownloadRate() float64 {
	return p.Stats().DownloadRate
}

func (cn *Peer) iterContiguousPieceRequests(f func(piece pieceIndex, count int)) {
	var last Option[pieceIndex]
	var count int
	next := func(item Option[pieceIndex]) {
		if item == last {
			count++
		} else {
			if count != 0 {
				f(last.Value, count)
			}
			last = item
			count = 1
		}
	}
	cn.requestState.Requests.Iterate(func(requestIndex request_strategy.RequestIndex) bool {
		next(Some(cn.t.pieceIndexOfRequestIndex(requestIndex)))
		return true
	})
	next(None[pieceIndex]())
}

func (cn *Peer) writeStatus(w io.Writer) {
	// \t isn't preserved in <pre> blocks?
	if cn.closed.IsSet() {
		fmt.Fprint(w, "CLOSED: ")
	}
	fmt.Fprintln(w, strings.Join(cn.peerImplStatusLines(), "\n"))
	prio, err := cn.peerPriority()
	prioStr := fmt.Sprintf("%08x", prio)
	if err != nil {
		prioStr += ": " + err.Error()
	}
	fmt.Fprintf(w, "bep40-prio: %v\n", prioStr)
	fmt.Fprintf(w, "last msg: %s, connected: %s, last helpful: %s, itime: %s, etime: %s\n",
		eventAgeString(cn.lastMessageReceived),
		eventAgeString(cn.completedHandshake),
		eventAgeString(cn.lastHelpful()),
		cn.cumInterest(),
		cn.totalExpectingTime(),
	)
	fmt.Fprintf(w,
		"%s completed, %d pieces touched, good chunks: %v/%v:%v reqq: %d+%v/(%d/%d):%d/%d, flags: %s, dr: %.1f KiB/s\n",
		cn.completedString(),
		len(cn.peerTouchedPieces),
		&cn._stats.ChunksReadUseful,
		&cn._stats.ChunksRead,
		&cn._stats.ChunksWritten,
		cn.requestState.Requests.GetCardinality(),
		cn.requestState.Cancelled.GetCardinality(),
		cn.nominalMaxRequests(),
		cn.PeerMaxRequests,
		len(cn.peerRequests),
		localClientReqq,
		cn.statusFlags(),
		cn.downloadRate()/(1<<10),
	)
	fmt.Fprintf(w, "requested pieces:")
	cn.iterContiguousPieceRequests(func(piece pieceIndex, count int) {
		fmt.Fprintf(w, " %v(%v)", piece, count)
	})
	fmt.Fprintf(w, "\n")
}

func (p *Peer) close() {
	if !p.closed.Set() {
		return
	}
	if p.updateRequestsTimer != nil {
		p.updateRequestsTimer.Stop()
	}
	for _, prs := range p.peerRequests {
		prs.allocReservation.Drop()
	}
	p.legacyPeerImpl.onClose()
	if p.t != nil {
		p.t.decPeerPieceAvailability(p)
	}
	for _, f := range p.callbacks.PeerClosed {
		f(p)
	}
}

func (p *Peer) Close() error {
	p.locker().Lock()
	defer p.locker().Unlock()
	p.close()
	return nil
}

// Peer definitely has a piece, for purposes of requesting. So it's not sufficient that we think
// they do (known=true).
func (cn *Peer) peerHasPiece(piece pieceIndex) bool {
	if all, known := cn.peerHasAllPieces(); all && known {
		return true
	}
	return cn.peerPieces().ContainsInt(piece)
}

// 64KiB, but temporarily less to work around an issue with WebRTC. TODO: Update when
// https://github.com/pion/datachannel/issues/59 is fixed.
const (
	writeBufferHighWaterLen = 1 << 17 // 128KiB - double the original 64KiB
	writeBufferLowWaterLen  = writeBufferHighWaterLen / 2
)

var (
	interestedMsgLen = len(pp.Message{Type: pp.Interested}.MustMarshalBinary())
	requestMsgLen    = len(pp.Message{Type: pp.Request}.MustMarshalBinary())
	// This is the maximum request count that could fit in the write buffer if it's at or below the
	// low water mark when we run maybeUpdateActualRequestState.
	maxLocalToRemoteRequests = (writeBufferHighWaterLen - writeBufferLowWaterLen - interestedMsgLen) / requestMsgLen
)

// The actual value to use as the maximum outbound requests.
func (cn *Peer) nominalMaxRequests() maxRequests {
	return maxInt(1, minInt(cn.PeerMaxRequests, cn.peakRequests*2, maxLocalToRemoteRequests))
}

func (cn *Peer) totalExpectingTime() (ret time.Duration) {
	ret = cn.cumulativeExpectedToReceiveChunks
	if !cn.lastStartedExpectingToReceiveChunks.IsZero() {
		ret += time.Since(cn.lastStartedExpectingToReceiveChunks)
	}
	return
}

func (cn *Peer) setInterested(interested bool) bool {
	if cn.requestState.Interested == interested {
		return true
	}
	cn.requestState.Interested = interested
	if interested {
		cn.lastBecameInterested = time.Now()
	} else if !cn.lastBecameInterested.IsZero() {
		cn.priorInterest += time.Since(cn.lastBecameInterested)
	}
	cn.updateExpectingChunks()
	// log.Printf("%p: setting interest: %v", cn, interested)
	return cn.writeInterested(interested)
}

// The function takes a message to be sent, and returns true if more messages
// are okay.
type messageWriter func(pp.Message) bool

// This function seems to only used by Peer.request. It's all logic checks, so maybe we can no-op it
// when we want to go fast.
func (cn *Peer) shouldRequest(r RequestIndex) error {
	err := cn.t.checkValidReceiveChunk(cn.t.requestIndexToRequest(r))
	if err != nil {
		return err
	}
	pi := cn.t.pieceIndexOfRequestIndex(r)
	if cn.requestState.Cancelled.Contains(r) {
		return errors.New("request is cancelled and waiting acknowledgement")
	}
	if !cn.peerHasPiece(pi) {
		return errors.New("requesting piece peer doesn't have")
	}
	if !cn.t.peerIsActive(cn) {
		panic("requesting but not in active conns")
	}
	if cn.closed.IsSet() {
		panic("requesting when connection is closed")
	}
	if cn.t.hashingPiece(pi) {
		panic("piece is being hashed")
	}
	if cn.t.pieceQueuedForHash(pi) {
		panic("piece is queued for hash")
	}
	if cn.peerChoking && !cn.peerAllowedFast.Contains(pi) {
		// This could occur if we made a request with the fast extension, and then got choked and
		// haven't had the request rejected yet.
		if !cn.requestState.Requests.Contains(r) {
			panic("peer choking and piece not allowed fast")
		}
	}
	return nil
}

func (cn *Peer) mustRequest(r RequestIndex) bool {
	more, err := cn.request(r)
	if err != nil {
		cn.logger.Printf("failed to make request %v: %v", r, err)
		return false
	}
	return more
}

func (cn *Peer) request(r RequestIndex) (more bool, err error) {
	if err := cn.shouldRequest(r); err != nil {
		return false, err
	}
	if cn.requestState.Requests.Contains(r) {
		return true, nil
	}
	if maxRequests(cn.requestState.Requests.GetCardinality()) >= cn.nominalMaxRequests() {
		return true, errors.New("too many outstanding requests")
	}
	cn.requestState.Requests.Add(r)
	if cn.validReceiveChunks == nil {
		cn.validReceiveChunks = make(map[RequestIndex]int)
	}
	cn.validReceiveChunks[r]++
	cn.t.requestState[r] = requestState{
		peer: cn,
		when: time.Now(),
	}
	cn.updateExpectingChunks()
	ppReq := cn.t.requestIndexToRequest(r)
	for _, f := range cn.callbacks.SentRequest {
		f(PeerRequestEvent{cn, ppReq})
	}
	return cn.legacyPeerImpl._request(ppReq), nil
}

func (me *Peer) cancel(r RequestIndex) {
	if !me.deleteRequest(r) {
		panic("request not existing should have been guarded")
	}
	if me._cancel(r) {
		// Record that we expect to get a cancel ack.
		if !me.requestState.Cancelled.CheckedAdd(r) {
			panic("request already cancelled")
		}
	}
	me.decPeakRequests()
	if me.isLowOnRequests() {
		me.updateRequests(peerUpdateRequestsPeerCancelReason)
	}
}

// Sets a reason to update requests, and if there wasn't already one, handle it.
func (cn *Peer) updateRequests(reason updateRequestReason) {
	if cn.needRequestUpdate != "" {
		return
	}
	cn.needRequestUpdate = reason
	cn.handleUpdateRequests()
}

// Emits the indices in the Bitmaps bms in order, never repeating any index.
// skip is mutated during execution, and its initial values will never be
// emitted.
func iterBitmapsDistinct(skip *bitmap.Bitmap, bms ...bitmap.Bitmap) iter.Func {
	return func(cb iter.Callback) {
		for _, bm := range bms {
			if !iter.All(
				func(_i interface{}) bool {
					i := _i.(int)
					if skip.Contains(bitmap.BitIndex(i)) {
						return true
					}
					skip.Add(bitmap.BitIndex(i))
					return cb(i)
				},
				bm.Iter,
			) {
				return
			}
		}
	}
}

// After handshake, we know what Torrent and Client stats to include for a
// connection.
func (cn *Peer) postHandshakeStats(f func(*ConnStats)) {
	t := cn.t
	f(&t.connStats)
	f(&t.cl.connStats)
}

// All ConnStats that include this connection. Some objects are not known
// until the handshake is complete, after which it's expected to reconcile the
// differences.
func (cn *Peer) allStats(f func(*ConnStats)) {
	f(&cn._stats)
	if cn.reconciledHandshakeStats {
		cn.postHandshakeStats(f)
	}
}

func (cn *Peer) readBytes(n int64) {
	cn.allStats(add(n, func(cs *ConnStats) *Count { return &cs.BytesRead }))
}

func (c *Peer) lastHelpful() (ret time.Time) {
	ret = c.lastUsefulChunkReceived
	if c.t.seeding() && c.lastChunkSent.After(ret) {
		ret = c.lastChunkSent
	}
	return
}

// Returns whether any part of the chunk would lie outside a piece of the given length.
func chunkOverflowsPiece(cs ChunkSpec, pieceLength pp.Integer) bool {
	switch {
	default:
		return false
	case cs.Begin+cs.Length > pieceLength:
	// Check for integer overflow
	case cs.Begin > pp.IntegerMax-cs.Length:
	}
	return true
}

func runSafeExtraneous(f func()) {
	if true {
		go f()
	} else {
		f()
	}
}

// Returns true if it was valid to reject the request.
func (c *Peer) remoteRejectedRequest(r RequestIndex) bool {
	if c.deleteRequest(r) {
		c.decPeakRequests()
	} else if !c.requestState.Cancelled.CheckedRemove(r) {
		return false
	}
	if c.isLowOnRequests() {
		c.updateRequests(peerUpdateRequestsRemoteRejectReason)
	}
	c.decExpectedChunkReceive(r)
	return true
}

func (c *Peer) decExpectedChunkReceive(r RequestIndex) {
	count := c.validReceiveChunks[r]
	if count == 1 {
		delete(c.validReceiveChunks, r)
	} else if count > 1 {
		c.validReceiveChunks[r] = count - 1
	} else {
		c.logger.Printf("unexpected chunk accounting for request %v: count=%d", r, count)
	}
}

func (c *Peer) doChunkReadStats(size int64) {
	c.allStats(func(cs *ConnStats) { cs.receivedChunk(size) })
}

// Handle a received chunk from a peer.
func (c *Peer) receiveChunk(msg *pp.Message, msgTime time.Time) error {
	return c.receiveChunkImpl(msg, msgTime, false)
}

// Handle a received chunk from a webseed peer with immediate piece state change notification
func (c *Peer) receiveChunkFromWebseed(msg *pp.Message, msgTime time.Time) error {
	return c.receiveChunkImpl(msg, msgTime, true)
}

func (c *Peer) receiveChunkImpl(msg *pp.Message, msgTime time.Time, immediate bool) error {
	if debugMetricsEnabled {
		ChunksReceived.Add("total", 1)
	}

	ppReq := newRequestFromMessage(msg)
	t := c.t
	err := t.checkValidReceiveChunk(ppReq)
	if err != nil {
		err = log.WithLevel(log.Warning, err)
		return err
	}
	req := c.t.requestIndexFromRequest(ppReq)

	recordBlockForSmartBan := sync.OnceFunc(func() {
		c.recordBlockForSmartBan(req, msg.Piece)
	})
	// This needs to occur before we return, but we try to do it when the client is unlocked. It
	// can't be done before checking if chunks are valid because they won't be deallocated by piece
	// hashing if they're out of bounds.
	defer recordBlockForSmartBan()

	if c.peerChoking {
		if debugMetricsEnabled {
			ChunksReceived.Add("while choked", 1)
		}
	}

	if c.validReceiveChunks[req] <= 0 {
		if debugMetricsEnabled {
			ChunksReceived.Add("unexpected", 1)
		}
		return errors.New("received unexpected chunk")
	}
	c.decExpectedChunkReceive(req)

	if c.peerChoking && c.peerAllowedFast.Contains(pieceIndex(ppReq.Index)) {
		if debugMetricsEnabled {
			ChunksReceived.Add("due to allowed fast", 1)
		}
	}

	// The request needs to be deleted immediately to prevent cancels occurring asynchronously when
	// have actually already received the piece, while we have the Client unlocked to write the data
	// out.
	intended := false
	{
		if c.requestState.Requests.Contains(req) {
			for _, f := range c.callbacks.ReceivedRequested {
				f(PeerMessageEvent{c, msg})
			}
		}
		// Request has been satisfied.
		if c.deleteRequest(req) || c.requestState.Cancelled.CheckedRemove(req) {
			intended = true
			if !c.peerChoking {
				c._chunksReceivedWhileExpecting++
			}
			if c.isLowOnRequests() {
				c.updateRequests("Peer.receiveChunk deleted request")
			}
		} else {
			if debugMetricsEnabled {
				ChunksReceived.Add("unintended", 1)
			}
		}
	}

	cl := t.cl

	// Do we actually want this chunk?
	if t.haveChunk(ppReq) {
		// panic(fmt.Sprintf("%+v", ppReq))
		if debugMetricsEnabled {
			ChunksReceived.Add("redundant", 1)
		}
		c.allStats(add(1, func(cs *ConnStats) *Count { return &cs.ChunksReadWasted }))
		return nil
	}

	piece := &t.pieces[ppReq.Index]

	// Direct atomic stats updates to eliminate function call overhead
	chunkSize := int64(len(msg.Piece))
	c._stats.ChunksReadUseful.Add(1)
	c._stats.BytesReadUsefulData.Add(chunkSize)
	if c.reconciledHandshakeStats {
		c.t.connStats.ChunksReadUseful.Add(1)
		c.t.connStats.BytesReadUsefulData.Add(chunkSize)
		c.t.cl.connStats.ChunksReadUseful.Add(1)
		c.t.cl.connStats.BytesReadUsefulData.Add(chunkSize)
	}
	if intended {
		c.piecesReceivedSinceLastRequestUpdate++
		c._stats.BytesReadUsefulIntendedData.Add(chunkSize)
		if c.reconciledHandshakeStats {
			c.t.connStats.BytesReadUsefulIntendedData.Add(chunkSize)
			c.t.cl.connStats.BytesReadUsefulIntendedData.Add(chunkSize)
		}
	}
	for _, f := range c.t.cl.config.Callbacks.ReceivedUsefulData {
		f(ReceivedUsefulDataEvent{c, msg})
	}
	c.lastUsefulChunkReceived = msgTime

	// Need to record that it hasn't been written yet, before we attempt to do
	// anything with it.
	piece.incrementPendingWrites()
	// Record that we have the chunk, so we aren't trying to download it while
	// waiting for it to be written to storage.
	piece.unpendChunkIndex(chunkIndexFromChunkSpec(ppReq.ChunkSpec, t.chunkSize))

	// Cancel pending requests for this chunk from *other* peers.
	if p := t.requestingPeer(req); p != nil {
		if p == c {
			panic("should not be pending request from conn that just received it")
		}
		p.cancel(req)
	}

	err = func() error {
		cl._mu.internal.Unlock() // Use internal unlock to bypass deferred actions
		defer cl._mu.internal.Lock() // Use internal lock to bypass deferred actions
		// Opportunistically do this here while we aren't holding the client lock.
		recordBlockForSmartBan()
		concurrentChunkWrites.Add(1)
		defer concurrentChunkWrites.Add(-1)
		// Write the chunk out. Note that the upper bound on chunk writing concurrency will be the
		// number of connections. We write inline with receiving the chunk (with this lock dance),
		// because we want to handle errors synchronously and I haven't thought of a nice way to
		// defer any concurrency to the storage and have that notify the client of errors. TODO: Do
		// that instead.
		return t.writeChunk(int(msg.Index), int64(msg.Begin), msg.Piece)
	}()

	piece.decrementPendingWrites()

	if err != nil {
		// c.logger.WithDefaultLevel(log.Error).Printf("writing received chunk %v: %v", req, err)
		t.pendRequest(req)
		// Necessary to pass TestReceiveChunkStorageFailureSeederFastExtensionDisabled. I think a
		// request update runs while we're writing the chunk that just failed. Then we never do a
		// fresh update after pending the failed request.
		c.updateRequests("Peer.receiveChunk error writing chunk")
		t.onWriteChunkErr(err)
		return nil
	}

	c.onDirtiedPiece(pieceIndex(ppReq.Index))

	// We need to ensure the piece is only queued once, so only the last chunk writer gets this job.
	if t.pieceAllDirty(pieceIndex(ppReq.Index)) && piece.pendingWrites == 0 {
		t.queuePieceCheck(pieceIndex(ppReq.Index))
		// We don't pend all chunks here anymore because we don't want code dependent on the dirty
		// chunk status (such as the haveChunk call above) to have to check all the various other
		// piece states like queued for hash, hashing etc. This does mean that we need to be sure
		// that chunk pieces are pended at an appropriate time later however.
	}

	cl.event.Broadcast()
	// We do this because we've written a chunk, and may change PieceState.Partial.
	if immediate {
		t.publishPieceStateChangeImmediate(pieceIndex(ppReq.Index))
	} else {
		t.publishPieceStateChange(pieceIndex(ppReq.Index))
	}

	return nil
}

func (c *Peer) onDirtiedPiece(piece pieceIndex) {
	if c.peerTouchedPieces == nil {
		c.peerTouchedPieces = make(map[pieceIndex]struct{})
	}
	c.peerTouchedPieces[piece] = struct{}{}
	ds := &c.t.pieces[piece].dirtiers
	if *ds == nil {
		*ds = make(map[*Peer]struct{})
	}
	(*ds)[c] = struct{}{}
}

func (cn *Peer) netGoodPiecesDirtied() int64 {
	return cn._stats.PiecesDirtiedGood.Int64() - cn._stats.PiecesDirtiedBad.Int64()
}

func (c *Peer) peerHasWantedPieces() bool {
	if all, _ := c.peerHasAllPieces(); all {
		return !c.t.haveAllPieces() && !c.t._pendingPieces.IsEmpty()
	}
	if !c.t.haveInfo() {
		return !c.peerPieces().IsEmpty()
	}
	return c.peerPieces().Intersects(&c.t._pendingPieces)
}

// Returns true if an outstanding request is removed. Cancelled requests should be handled
// separately.
func (c *Peer) deleteRequest(r RequestIndex) bool {
	if !c.requestState.Requests.CheckedRemove(r) {
		return false
	}
	for _, f := range c.callbacks.DeletedRequest {
		f(PeerRequestEvent{c, c.t.requestIndexToRequest(r)})
	}
	c.updateExpectingChunks()
	if c.t.requestingPeer(r) != c {
		panic("only one peer should have a given request at a time")
	}
	delete(c.t.requestState, r)
	// c.t.iterPeers(func(p *Peer) {
	// 	if p.isLowOnRequests() {
	// 		p.updateRequests("Peer.deleteRequest")
	// 	}
	// })
	return true
}

func (c *Peer) deleteAllRequests(reason updateRequestReason) {
	if c.requestState.Requests.IsEmpty() {
		return
	}
	c.requestState.Requests.IterateSnapshot(func(x RequestIndex) bool {
		if !c.deleteRequest(x) {
			panic("request should exist")
		}
		return true
	})
	c.assertNoRequests()
	c.t.iterPeers(func(p *Peer) {
		if p.isLowOnRequests() {
			p.updateRequests(reason)
		}
	})
	return
}

func (c *Peer) assertNoRequests() {
	if !c.requestState.Requests.IsEmpty() {
		panic(c.requestState.Requests.GetCardinality())
	}
}

func (c *Peer) cancelAllRequests() {
	c.requestState.Requests.IterateSnapshot(func(x RequestIndex) bool {
		c.cancel(x)
		return true
	})
	c.assertNoRequests()
	return
}

func (c *Peer) peerPriority() (peerPriority, error) {
	return bep40Priority(c.remoteIpPort(), c.localPublicAddr)
}

func (c *Peer) remoteIp() net.IP {
	if c.RemoteAddr == nil {
		return nil
	}
	host, _, _ := net.SplitHostPort(c.RemoteAddr.String())
	return net.ParseIP(host)
}

func (c *Peer) remoteIpPort() IpPort {
	ipa, _ := tryIpPortFromNetAddr(c.RemoteAddr)
	return IpPort{ipa.IP, uint16(ipa.Port)}
}

func (c *Peer) trust() connectionTrust {
	return connectionTrust{c.trusted, c.netGoodPiecesDirtied()}
}

type connectionTrust struct {
	Implicit            bool
	NetGoodPiecesDirted int64
}

func (l connectionTrust) Cmp(r connectionTrust) int {
	return multiless.New().Bool(l.Implicit, r.Implicit).Int64(l.NetGoodPiecesDirted, r.NetGoodPiecesDirted).OrderingInt()
}

// Returns a new Bitmap that includes bits for all pieces the peer could have based on their claims.
func (cn *Peer) newPeerPieces() *roaring.Bitmap {
	// TODO: Can we use copy on write?
	ret := cn.peerPieces().Clone()
	if all, _ := cn.peerHasAllPieces(); all {
		if cn.t.haveInfo() {
			ret.AddRange(0, bitmap.BitRange(cn.t.numPieces()))
		} else {
			ret.AddRange(0, bitmap.ToEnd)
		}
	}
	return ret
}

func (cn *Peer) stats() *ConnStats {
	return &cn._stats
}

func (p *Peer) TryAsPeerConn() (*PeerConn, bool) {
	pc, ok := p.legacyPeerImpl.(*PeerConn)
	return pc, ok
}

func (p *Peer) uncancelledRequests() uint64 {
	return p.requestState.Requests.GetCardinality()
}

type peerLocalPublicAddr = IpPort

func (p *Peer) isLowOnRequests() bool {
	return p.requestState.Requests.IsEmpty() && p.requestState.Cancelled.IsEmpty()
}

func (p *Peer) decPeakRequests() {
	// // This can occur when peak requests are altered by the update request timer to be lower than
	// // the actual number of outstanding requests. Let's let it go negative and see what happens. I
	// // wonder what happens if maxRequests is not signed.
	// if p.peakRequests < 1 {
	// 	panic(p.peakRequests)
	// }
	p.peakRequests--
}

func (p *Peer) recordBlockForSmartBan(req RequestIndex, blockData []byte) {
	if p.bannableAddr.Ok {
		p.t.smartBanCache.RecordBlock(p.bannableAddr.Value, req, blockData)
	}
}
