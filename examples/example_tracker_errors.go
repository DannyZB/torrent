package main

import (
	"fmt"
	"log"
	"time"

	"github.com/anacrolix/torrent"
)

// Example showing how to monitor tracker errors per-torrent
func main() {
	// Create a torrent client
	config := torrent.NewDefaultClientConfig()
	client, err := torrent.NewClient(config)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	// Add a torrent (replace with actual torrent file or magnet link)
	// t, err := client.AddMagnet("magnet:?xt=urn:btih:...")
	// if err != nil {
	// 	log.Fatal(err)
	// }

	// For demonstration, let's assume we have a torrent
	torrents := client.Torrents()
	if len(torrents) == 0 {
		fmt.Println("No torrents available for monitoring")
		return
	}

	t := torrents[0]

	// Monitor tracker statuses
	monitorTrackerErrors(t)
}

func monitorTrackerErrors(t *torrent.Torrent) {
	fmt.Printf("Monitoring tracker errors for torrent: %s\n", t.Name())
	
	for {
		statuses := t.TrackerStatuses()
		
		fmt.Printf("\n=== Tracker Status Report (Time: %s) ===\n", time.Now().Format("15:04:05"))
		
		if len(statuses) == 0 {
			fmt.Println("No trackers configured")
		}
		
		for i, status := range statuses {
			fmt.Printf("\nTracker %d: %s\n", i+1, status.URL)
			
			if status.IsWorking() {
				fmt.Printf("  ✓ Status: Working\n")
				fmt.Printf("  ✓ Last announce: %s\n", status.LastAnnounce.Format("15:04:05"))
				fmt.Printf("  ✓ Peers returned: %d\n", status.NumPeers)
				fmt.Printf("  ✓ Announce interval: %s\n", status.Interval)
				if !status.NextAnnounce.IsZero() {
					fmt.Printf("  ✓ Next announce: %s\n", status.NextAnnounce.Format("15:04:05"))
				}
			} else if status.LastError != nil {
				fmt.Printf("  ✗ Status: ERROR\n")
				fmt.Printf("  ✗ Error: %s\n", status.LastError.Error())
				fmt.Printf("  ✗ Error type: %s\n", status.ErrorType())
				
				// Provide specific guidance based on error type
				switch status.ErrorType() {
				case "torrent_not_registered":
					fmt.Printf("  💡 Suggestion: The torrent may have been removed from this tracker or expired\n")
				case "tracker_not_found":
					fmt.Printf("  💡 Suggestion: Check if the tracker URL is correct (404 error)\n")
				case "tracker_unavailable":
					fmt.Printf("  💡 Suggestion: Tracker is temporarily down (503 error), will retry automatically\n")
				case "tracker_http_error":
					fmt.Printf("  💡 Suggestion: Tracker returned HTTP error, check tracker status\n")
				case "tracker_failure":
					fmt.Printf("  💡 Suggestion: Tracker-specific failure, check tracker requirements\n")
				case "authentication_failed":
					fmt.Printf("  💡 Suggestion: Check passkey or account status for private trackers\n")
				case "dns_error":
					fmt.Printf("  💡 Suggestion: DNS resolution failed, check network settings\n")
				case "timeout":
					fmt.Printf("  💡 Suggestion: Network or tracker response is slow\n")
				case "cancelled":
					fmt.Printf("  💡 Suggestion: Announce was cancelled, usually during shutdown\n")
				case "network_error":
					fmt.Printf("  💡 Suggestion: Check network connectivity\n")
				case "udp_connection_error":
					fmt.Printf("  💡 Suggestion: UDP tracker connection issue, may retry\n")
				case "client_closed":
					fmt.Printf("  💡 Suggestion: Client was closed during announce\n")
				default:
					fmt.Printf("  💡 Suggestion: Unknown error type: %s\n", status.ErrorType())
				}
				
				if !status.LastAnnounce.IsZero() {
					fmt.Printf("  ⏰ Last successful announce: %s\n", status.LastAnnounce.Format("15:04:05"))
				}
			} else {
				fmt.Printf("  ⏳ Status: No announce attempted yet\n")
			}
		}
		
		// Check if any trackers are working
		workingTrackers := 0
		for _, status := range statuses {
			if status.IsWorking() {
				workingTrackers++
			}
		}
		
		fmt.Printf("\n📊 Summary: %d/%d trackers working\n", workingTrackers, len(statuses))
		
		if workingTrackers == 0 && len(statuses) > 0 {
			fmt.Printf("⚠️  WARNING: No trackers are working! Torrent may have trouble finding peers.\n")
		}
		
		// Wait before next check
		time.Sleep(30 * time.Second)
	}
}

// Example function to categorize and count errors across all torrents
func analyzeTrackerErrors(client *torrent.Client) {
	errorCounts := make(map[string]int)
	totalTrackers := 0
	workingTrackers := 0
	
	for _, t := range client.Torrents() {
		statuses := t.TrackerStatuses()
		totalTrackers += len(statuses)
		
		for _, status := range statuses {
			if status.IsWorking() {
				workingTrackers++
			} else if status.LastError != nil {
				errorType := status.ErrorType()
				errorCounts[errorType]++
			}
		}
	}
	
	fmt.Printf("\n=== Global Tracker Error Analysis ===\n")
	fmt.Printf("Total trackers: %d\n", totalTrackers)
	fmt.Printf("Working trackers: %d (%.1f%%)\n", workingTrackers, float64(workingTrackers)/float64(totalTrackers)*100)
	
	if len(errorCounts) > 0 {
		fmt.Printf("\nError breakdown:\n")
		for errorType, count := range errorCounts {
			fmt.Printf("  %s: %d\n", errorType, count)
		}
	}
} 