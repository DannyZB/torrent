package requestStrategy

import (
	"github.com/ajwerner/btree"
)

type ajwernerBtree struct {
	btree btree.Set[PieceRequestOrderItem]
}

func (a *ajwernerBtree) Contains(item PieceRequestOrderItem) bool {
	_, ok := a.btree.Get(item)
	return ok
}

var _ Btree = (*ajwernerBtree)(nil)

func NewAjwernerBtree() *ajwernerBtree {
	return &ajwernerBtree{
		btree: btree.MakeSet(func(t, t2 PieceRequestOrderItem) int {
			return pieceOrderLess(&t, &t2).OrderingInt()
		}),
	}
}

func (a *ajwernerBtree) Delete(item PieceRequestOrderItem) {
	a.btree.Delete(item)
}

func (a *ajwernerBtree) Add(item PieceRequestOrderItem) {
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
