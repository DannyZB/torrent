package torrent

import "io"

func (t *Torrent) storageReader() storageReader {
	return storagePieceReader{t: t}
}

type storageReader interface {
	io.ReaderAt
	io.Closer
}

type storagePieceReader struct {
	t *Torrent
}

func (storagePieceReader) Close() error { return nil }

func (me storagePieceReader) ReadAt(b []byte, off int64) (n int, err error) {
	for len(b) > 0 {
		piece := me.t.pieceForOffset(off)
		piece.waitNoPendingWrites()
		info := piece.Info()
		pieceOffset := off - info.Offset()
		pieceLen := info.Length()
		if pieceOffset >= pieceLen {
			err = io.EOF
			return
		}
		max := pieceLen - pieceOffset
		if int64(len(b)) < max {
			max = int64(len(b))
		}
		storagePiece := piece.Storage()
		n1, err1 := storagePiece.ReadAt(b[:max], pieceOffset)
		n += n1
		off += int64(n1)
		b = b[n1:]
		if err1 != nil {
			if err1 == io.EOF && len(b) > 0 {
				err = io.ErrUnexpectedEOF
			} else {
				err = err1
			}
			return
		}
		if int64(n1) < max {
			err = io.ErrUnexpectedEOF
			return
		}
	}
	return
}
