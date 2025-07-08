// Package version provides default versions, user-agents etc. for client identification.
package version

var (
	DefaultExtendedHandshakeClientVersion string
	// This should be updated when client behaviour changes in a way that other peers could care
	// about.
	DefaultBep20Prefix   = "-DE211s-"
	DefaultHttpUserAgent string
	DefaultUpnpId        string
)

func init() {
	DefaultExtendedHandshakeClientVersion = "Deluge 2.1.1"
	DefaultUpnpId = "Deluge 2.1.1"
	// Per https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/User-Agent#library_and_net_tool_ua_strings
	DefaultHttpUserAgent = "Deluge/2.1.1 libtorrent/1.2.19.0"
}
