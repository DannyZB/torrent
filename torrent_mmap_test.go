//go:build !wasm
// +build !wasm

package torrent

import (
	"testing"

	"github.com/dannyzb/torrent/storage"
)

func TestEmptyFilesAndZeroPieceLengthWithMMapStorage(t *testing.T) {
	cfg := TestingConfig(t)
	ci := storage.NewMMap(cfg.DataDir)
	defer ci.Close()
	cfg.DefaultStorage = ci
	testEmptyFilesAndZeroPieceLength(t, cfg)
}
