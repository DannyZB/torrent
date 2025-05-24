# Tracker Error Reporting

This fork adds comprehensive tracker error reporting functionality to the anacrolix/torrent library, allowing you to monitor and diagnose tracker issues on a per-torrent basis.

## Overview

Previously, tracker error information was only available through internal logging. This enhancement exposes detailed tracker status information through a public API, enabling applications to:

- Monitor tracker health in real-time
- Categorize and respond to different types of tracker failures
- Provide better user feedback about connectivity issues
- Implement retry logic and tracker management strategies

## API Reference

### TrackerStatus struct

```go
type TrackerStatus struct {
    // URL of the tracker
    URL string
    // LastError contains the most recent error from this tracker, nil if last announce was successful
    LastError error
    // LastAnnounce is the time of the last announce attempt
    LastAnnounce time.Time
    // NumPeers is the number of peers returned by the last successful announce
    NumPeers int
    // Interval is the announce interval suggested by the tracker
    Interval time.Duration
    // NextAnnounce is the calculated time for the next announce
    NextAnnounce time.Time
}
```

### Methods

#### `(t *Torrent) TrackerStatuses() []TrackerStatus`

Returns the current status of all trackers configured for this torrent.

#### `(ts TrackerStatus) IsWorking() bool`

Returns true if the tracker is responding without errors.

#### `(ts TrackerStatus) ErrorType() string`

-Returns a categorized error type based on actual tracker failures encountered by this library. Possible values:

- `"tracker_not_found"` - HTTP 404 errors from tracker
- `"tracker_unavailable"` - HTTP 503 or server unavailable errors  
- `"authentication_failed"` - HTTP 401/403 or tracker auth failures
- `"tracker_http_error"` - Other HTTP status code errors
- `"tracker_failure"` - Tracker-specific failure reasons
- `"torrent_not_registered"` - Torrent not found/registered on tracker
- `"dns_error"` - DNS resolution failures
- `"client_closed"` - Client shutdown during announce
- `"timeout"` - Context deadline exceeded or timeouts
- `"cancelled"` - Context cancellation
- `"network_error"` - Network connectivity issues
- `"udp_connection_error"` - UDP tracker connection issues
- `"unknown_error"` - Other/unrecognized errors

## Performance Optimizations

This fork also includes several performance improvements:

### 1. Connection Management Optimization
- **worstBadConn()**: Reduced from O(n log n) to O(n) complexity
- Eliminated heap operations for connection ranking
- Faster peer connection cleanup and management

### 2. Piece Management Optimization  
- **numDirtyBytes()**: Reduced memory allocations and computation
- **unpendChunkRange()**: Added batch operations for chunk state changes
- More efficient dirty chunk tracking

### 3. Error Categorization
- **ErrorType()**: Uses actual error patterns from the library instead of generic strings
- More accurate error classification based on real tracker responses

## Usage Examples

### Basic Tracker Status Monitoring

```go
// Get tracker statuses for a torrent
statuses := torrent.TrackerStatuses()

for _, status := range statuses {
    if status.IsWorking() {
        fmt.Printf("✓ %s: %d peers\n", status.URL, status.NumPeers)
    } else if status.LastError != nil {
        fmt.Printf("✗ %s: %s (%s)\n", status.URL, status.LastError.Error(), status.ErrorType())
    }
}
```

### Error Categorization and Response

```go
for _, status := range torrent.TrackerStatuses() {
    if status.LastError != nil {
        switch status.ErrorType() {
        case "torrent_not_registered":
            log.Printf("Torrent expired on tracker: %s", status.URL)
        case "authentication_failed":
            log.Printf("Auth failed for tracker: %s", status.URL)
        case "tracker_unavailable":
            log.Printf("Tracker temporarily down: %s", status.URL)
        case "dns_error":
            log.Printf("DNS resolution failed for tracker: %s", status.URL)
        case "timeout":
            log.Printf("Tracker announce timed out: %s", status.URL)
        }
    }
}
```

### Health Monitoring

```go
workingTrackers := 0
totalTrackers := len(statuses)

for _, status := range statuses {
    if status.IsWorking() {
        workingTrackers++
    }
}

healthPercent := float64(workingTrackers) / float64(totalTrackers) * 100
fmt.Printf("Tracker health: %.1f%% (%d/%d working)\n", healthPercent, workingTrackers, totalTrackers)
```

## Common Tracker Error Types and Solutions

### HTTP Status Code Errors

**404 Not Found**: Incorrect tracker URL or the tracker has moved.
**Solution**: Verify the tracker URL is correct, check for redirects.

**503 Service Unavailable**: Tracker is overloaded or in maintenance mode.
**Solution**: Wait and retry later. The library will automatically retry.

**401/403 Unauthorized/Forbidden**: Authentication issues with private trackers.
**Solution**: Check credentials, ensure client is whitelisted.

### Tracker-Specific Failures

**"torrent not registered"**: The torrent has been removed from the tracker.
**Solution**: Remove the dead tracker or find a replacement.

**"passkey invalid"**: Private tracker authentication failure.
**Solution**: Update passkey, check account status.

### Network Errors

**DNS resolution failures**: Can't resolve tracker hostname.
**Solution**: Check DNS settings, verify hostname.

**Connection refused/Network unreachable**: Network connectivity issues.
**Solution**: Check internet connection, firewall settings.

**Timeouts**: Slow network or overloaded tracker.
**Solution**: Consider longer timeout, check network speed.

## Integration Tips

1. **Polling Frequency**: Check tracker status every 30-60 seconds
2. **Error Persistence**: Track error duration to distinguish temporary vs persistent issues
3. **User Feedback**: Use categorized error types for meaningful error messages
4. **Automatic Cleanup**: Remove persistently failing trackers automatically
5. **Fallback Strategy**: Ensure at least one working tracker or rely on DHT

## Example Implementation

See `examples/example_tracker_errors.go` for a complete example showing:
- Real-time tracker monitoring
- Error categorization and user-friendly messages
- Health summary across all trackers
- Suggested actions based on error types

## Compatibility

This enhancement maintains full backward compatibility with existing anacrolix/torrent code. The new functionality is purely additive and doesn't modify existing behavior. 