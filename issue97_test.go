package torrent

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dannyzb/torrent/internal/testutil"
	"github.com/dannyzb/torrent/metainfo"
	"github.com/dannyzb/torrent/storage"
)

func TestHashPieceAfterStorageClosed(t *testing.T) {
	cl := newTestingClient(t)
	td := t.TempDir()
	cs := storage.NewFile(td)
	defer cs.Close()
	tt := cl.newTorrent(metainfo.Hash{1}, cs)
	mi := testutil.GreetingMetaInfo()
	info, err := mi.UnmarshalInfo()
	require.NoError(t, err)
	require.NoError(t, tt.setInfo(&info))
	require.NoError(t, tt.storage.Close())
	tt.hashPiece(0)
}
