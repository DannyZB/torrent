package torrent

import (
	"fmt"

	"github.com/RoaringBitmap/roaring"
	g "github.com/anacrolix/generics"
	requestStrategy "github.com/anacrolix/torrent/internal/request-strategy"
)

// It's probably possible to track whether the piece moves around in the btree to be more efficient
// about triggering request updates.
// IMPORTANT: Caller must hold the client lock (cl.lock()) before calling this function.
func (t *Torrent) updatePieceRequestOrderPiece(pieceIndex int) (changed bool) {
	if t.storage == nil {
		return false
	}
	pro, ok := t.cl.pieceRequestOrder[t.clientPieceRequestOrderKey()]
	if !ok || pro == nil {
		return false
	}
	key := t.pieceRequestOrderKey(pieceIndex)
	if t.hasStorageCap() {
		return pro.Update(key, t.requestStrategyPieceOrderState(pieceIndex))
	}
	// TODO: This might eject a piece that could count toward being unverified?
	pending := !t.ignorePieceForRequests(pieceIndex)
	if pending {
		newState := t.requestStrategyPieceOrderState(pieceIndex)
		old := pro.Add(key, newState)
		return old.Ok && old.Value != newState
	} else {
		return pro.Delete(key)
	}
}

func (t *Torrent) clientPieceRequestOrderKey() clientPieceRequestOrderKeySumType {
	if t.storage.Capacity == nil {
		return clientPieceRequestOrderRegularTorrentKey{t}
	}
	return clientPieceRequestOrderSharedStorageTorrentKey{t.storage.Capacity}
}

// deletePieceRequestOrder removes all pieces from the piece request order.
// IMPORTANT: Caller must hold the client lock (cl.lock()) before calling this function.
func (t *Torrent) deletePieceRequestOrder() {
	if t.storage == nil {
		return
	}
	cpro := t.cl.pieceRequestOrder
	key := t.clientPieceRequestOrderKey()
	pro := cpro[key]
	if pro == nil {
		return
	}
	for i := 0; i < t.numPieces(); i++ {
		pro.Delete(t.pieceRequestOrderKey(i))
	}
	if pro.Len() == 0 {
		delete(cpro, key)
	}
}

// initPieceRequestOrder initializes the piece request order for the torrent.
// IMPORTANT: Caller must hold the client lock (cl.lock()) before calling this function.
func (t *Torrent) initPieceRequestOrder() {
	if t.storage == nil {
		return
	}
	g.MakeMapIfNil(&t.cl.pieceRequestOrder)
	key := t.clientPieceRequestOrderKey()
	cpro := t.cl.pieceRequestOrder
	if cpro[key] == nil {
		cpro[key] = requestStrategy.NewPieceOrder(requestStrategy.NewAjwernerBtree(), t.numPieces())
	}
}

func (t *Torrent) addRequestOrderPiece(i int) {
	if t.storage == nil {
		return
	}
	pro := t.getPieceRequestOrder()
	if pro == nil {
		return
	}
	key := t.pieceRequestOrderKey(i)
	if t.hasStorageCap() || !t.ignorePieceForRequests(i) {
		pro.Add(key, t.requestStrategyPieceOrderState(i))
	}
}

func (t *Torrent) getPieceRequestOrder() *requestStrategy.PieceRequestOrder {
	if t.storage == nil {
		return nil
	}
	return t.cl.pieceRequestOrder[t.clientPieceRequestOrderKey()]
}

func (t *Torrent) checkPendingPiecesMatchesRequestOrder() {
	short := *t.canonicalShortInfohash()
	var proBitmap roaring.Bitmap
	pro := t.getPieceRequestOrder()
	if pro == nil {
		return
	}
	if t.dataDownloadDisallowed.Bool() {
		return
	}
	for item := range pro.Iter() {
		if item.Key.InfoHash.Value() != short {
			continue
		}
		if item.State.Priority == PiecePriorityNone {
			continue
		}
		if t.ignorePieceForRequests(item.Key.Index) {
			continue
		}
		proBitmap.Add(uint32(item.Key.Index))
	}
	if !proBitmap.Equals(&t._pendingPieces) {
		intersection := roaring.And(&proBitmap, &t._pendingPieces)
		exclPro := roaring.AndNot(&proBitmap, intersection)
		exclPending := roaring.AndNot(&t._pendingPieces, intersection)
		panic(fmt.Sprintf("piece request order has %v and pending pieces has %v", exclPro.String(), exclPending.String()))
	}
}
