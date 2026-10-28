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
// The limits are generous on purpose: on a bad network a ping answered after
// 5 s was killing connections whose streams were only waiting for a slow
// backend (streams cut at exactly 20 s, 15 + 5, with no content yet).
// Vars so tests can shorten them.
var (
	healthPing    = 30 * time.Second // quiet this long: send a ping
	healthTimeout = 15 * time.Second // no answer this long: close the connection
)

// HealthLimit is how long a connection can be silent before a health ping
// closes it: a failure after about this long on a quiet connection is that.
func HealthLimit() time.Duration { return healthPing + healthTimeout }

// Harden turns on the health pings for tr's HTTP/2 connections, unless tr
// already carries its own HTTP/2 settings.
func Harden(tr *http.Transport, ping, timeout time.Duration) {
	if tr.HTTP2 != nil {
		return
	}
	tr.HTTP2 = &http.HTTP2Config{SendPingTimeout: ping, PingTimeout: timeout}
}
