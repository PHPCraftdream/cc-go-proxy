package connpool

import (
	"net/http"
	"time"
)

// Health pings of the HTTP/2 connections. Something on the way (a proxy
// program, an antivirus, a NAT) kills connections that have been quiet for a
// while; the client learns it only when the next request fails to write.
// With pings a quiet connection is never silent for long, and one that no
// longer answers is closed and leaves the pool before a request picks it.
// Vars so tests can shorten them.
var (
	healthPing    = 15 * time.Second // quiet this long: send a ping
	healthTimeout = 5 * time.Second  // no answer this long: close the connection
)

// Harden turns on the health pings for tr's HTTP/2 connections, unless tr
// already carries its own HTTP/2 settings.
func Harden(tr *http.Transport, ping, timeout time.Duration) {
	if tr.HTTP2 != nil {
		return
	}
	tr.HTTP2 = &http.HTTP2Config{SendPingTimeout: ping, PingTimeout: timeout}
}
