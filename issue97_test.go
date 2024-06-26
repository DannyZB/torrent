package torrent

import (
	"testing"

	"github.com/anacrolix/log"
	"github.com/stretchr/testify/require"

	"github.com/dannyzb/torrent/internal/testutil"
	"github.com/dannyzb/torrent/storage"
)

func TestHashPieceAfterStorageClosed(t *testing.T) {
	td := t.TempDir()
	cs := storage.NewFile(td)
	defer cs.Close()
	tt := &Torrent{
		storageOpener: storage.NewClient(cs),
		logger:        log.Default,
		chunkSize:     defaultChunkSize,
	}
	tt.infoHash.Ok = true
	tt.infoHash.Value[0] = 1
	mi := testutil.GreetingMetaInfo()
	info, err := mi.UnmarshalInfo()
	require.NoError(t, err)
	require.NoError(t, tt.setInfo(&info))
	require.NoError(t, tt.storage.Close())
	tt.hashPiece(0)
}
