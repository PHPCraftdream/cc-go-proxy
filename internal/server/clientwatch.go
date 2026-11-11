package server

import (
	"errors"
	"net"
	"os"
	"sync"
	"time"
)

// clientWatchTick is how often the watch looks at the client's connection.
const clientWatchTick = 500 * time.Millisecond

// watchClientGone calls gone once the client closes conn while a request is
// handled. It reads the connection, idle meanwhile: Claude Code sends nothing
// before the reply (no pipelining), and the reply closes the connection, so
// nothing a later request would need is taken. Only a TCP connection is
// watched (tests serve requests over in-memory ones). stop ends the watch and
// leaves the connection without a read deadline.
func watchClientGone(conn net.Conn, gone func()) (stop func()) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return func() {}
	}
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var b [1]byte
		for {
			select {
			case <-quit:
				return
			default:
			}
			_ = tcp.SetReadDeadline(time.Now().Add(clientWatchTick))
			n, err := tcp.Read(b[:])
			switch {
			case n > 0:
				return // data, not a close: nothing more to tell
			case err == nil, errors.Is(err, os.ErrDeadlineExceeded):
				continue
			default:
				select {
				case <-quit: // the deadline set by stop
				default:
					gone()
				}
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(quit)
			_ = tcp.SetReadDeadline(time.Now())
			<-done
			_ = tcp.SetReadDeadline(time.Time{})
		})
	}
}
