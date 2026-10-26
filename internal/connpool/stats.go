package connpool

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// Stats counts what a Transport did since the previous Stats call.
type Stats struct {
	Dials          int64 // connections opened by connpool's TLS dialer (warm-ups included)
	H2, HTTP1      int64 // the protocol they agreed on
	WarmOpened     int64 // warm-ups that connected
	WarmUsed       int64 // warm connections handed to a request
	WarmFailed     int64 // warm-ups that could not connect
	ConnectRetries int64 // requests sent again after their connection could not open
	BreakRetries   int64 // requests sent again after their connection broke before the reply
}

// String is the log form; "" when nothing happened.
func (s Stats) String() string {
	if s == (Stats{}) {
		return ""
	}
	var parts []string
	if s.Dials > 0 {
		parts = append(parts, fmt.Sprintf("dials %d (h2 %d, http/1.1 %d)", s.Dials, s.H2, s.HTTP1))
	}
	if s.WarmOpened+s.WarmUsed+s.WarmFailed > 0 {
		parts = append(parts, fmt.Sprintf("warm %d opened, %d used, %d failed", s.WarmOpened, s.WarmUsed, s.WarmFailed))
	}
	if s.ConnectRetries+s.BreakRetries > 0 {
		parts = append(parts, fmt.Sprintf("retries %d after connect failures, %d after broken connections", s.ConnectRetries, s.BreakRetries))
	}
	return strings.Join(parts, "; ")
}

type counters struct {
	dials, h2, http1, warmOpened, warmUsed, warmFailed, connectRetries, breakRetries atomic.Int64
}

func (c *counters) dialed(proto string) {
	c.dials.Add(1)
	if proto == "h2" {
		c.h2.Add(1)
	} else {
		c.http1.Add(1)
	}
}

// Stats returns the counts since the previous call and starts over.
func (t *Transport) Stats() Stats {
	c := &t.counts
	return Stats{
		Dials: c.dials.Swap(0), H2: c.h2.Swap(0), HTTP1: c.http1.Swap(0),
		WarmOpened: c.warmOpened.Swap(0), WarmUsed: c.warmUsed.Swap(0), WarmFailed: c.warmFailed.Swap(0),
		ConnectRetries: c.connectRetries.Swap(0), BreakRetries: c.breakRetries.Swap(0),
	}
}

func (s *Stats) add(o Stats) {
	s.Dials += o.Dials
	s.H2 += o.H2
	s.HTTP1 += o.HTTP1
	s.WarmOpened += o.WarmOpened
	s.WarmUsed += o.WarmUsed
	s.WarmFailed += o.WarmFailed
	s.ConnectRetries += o.ConnectRetries
	s.BreakRetries += o.BreakRetries
}
