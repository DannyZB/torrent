package requestStrategy

import (
	"github.com/ajwerner/btree"
)

type ajwernerBtree struct {
	btree btree.Set[PieceRequestOrderItem]
}

var _ Btree = (*ajwernerBtree)(nil)

func NewAjwernerBtree() *ajwernerBtree {
	return &ajwernerBtree{
		btree: btree.MakeSet(func(t, t2 PieceRequestOrderItem) int {
			return pieceOrderLess(&t, &t2).OrderingInt()
		}),
	}
}

func mustValue[V any](b bool, panicValue V) {
	if !b {
		panic(panicValue)
	}
}

func (a *ajwernerBtree) Delete(item PieceRequestOrderItem) {
	// Delete is idempotent - returns false if item doesn't exist
	a.btree.Delete(item)
}

func (a *ajwernerBtree) Add(item PieceRequestOrderItem) {
	// Upsert is idempotent - replaces existing items
	a.btree.Upsert(item)
}

func (a *ajwernerBtree) Scan(f func(PieceRequestOrderItem) bool) {
	it := a.btree.Iterator()
	it.First()
	for it.First(); it.Valid(); it.Next() {
		if !f(it.Cur()) {
			break
		}
	}
}
