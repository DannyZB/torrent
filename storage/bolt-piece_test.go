package storage_test

import (
	"testing"

	"github.com/dannyzb/torrent/storage"
	"github.com/dannyzb/torrent/test"
)

func TestBoltLeecherStorage(t *testing.T) {
	test.TestLeecherStorage(t, test.LeecherStorageTestCase{"Boltdb", storage.NewBoltDB, 0})
}
