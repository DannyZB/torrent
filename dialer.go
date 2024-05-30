package torrent

import (
	"github.com/dannyzb/torrent/dialer"
)

type (
	Dialer        = dialer.T
	NetworkDialer = dialer.WithNetwork
)

var DefaultNetDialer = &dialer.Default
