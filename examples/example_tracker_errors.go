package main

import (
	"fmt"
	"log"
	"time"

	"github.com/anacrolix/torrent"
)

func main() {
	cfg := torrent.NewDefaultClientConfig()
	cfg.Debug = true
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer cl.Close()

	if len(cl.Torrents()) == 0 {
		fmt.Println("no torrents to monitor; add one before running this example")
		return
	}

	t := cl.Torrents()[0]
	for {
		fmt.Printf("\n[%s] tracker status for %q\n", time.Now().Format(time.RFC3339), t.Name())
		statuses := t.TrackerStatuses()
		if len(statuses) == 0 {
			fmt.Println("  (no trackers configured)")
		}
		for _, st := range statuses {
			if st.IsWorking() {
				fmt.Printf("  ✓ %s peers=%d seeders=%d leechers=%d next=%s\n",
					st.URL,
					st.NumPeers,
					st.Seeders,
					st.Leechers,
					st.NextAnnounce.Format(time.Kitchen))
				continue
			}
			errType := st.ErrorType()
			fmt.Printf("  ✗ %s error=%s msg=%v\n", st.URL, errType, st.LastError)
		}
		time.Sleep(30 * time.Second)
	}
}
