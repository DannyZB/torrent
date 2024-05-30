//go:build !wasm
// +build !wasm

package torrent

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/dannyzb/torrent/internal/testutil"
	"github.com/dannyzb/torrent/storage"
)

func TestDropTorrentWithMmapStorageWhileHashing(t *testing.T) {
	cfg := TestingConfig(t)
	// Ensure the data is present when the torrent is added, and not obtained
	// over the network as the test runs.
	cfg.DownloadRateLimiter = rate.NewLimiter(0, 0)
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	defer cl.Close()

	td, mi := testutil.GreetingTestTorrent()
	mms := storage.NewMMap(td)
	defer mms.Close()
	tt, new, err := cl.AddTorrentSpec(&TorrentSpec{
		Storage:   mms,
		InfoHash:  mi.HashInfoBytes(),
		InfoBytes: mi.InfoBytes,
	})
	require.NoError(t, err)
	assert.True(t, new)

	r := tt.NewReader()
	go tt.Drop()
	io.Copy(io.Discard, r)
}
