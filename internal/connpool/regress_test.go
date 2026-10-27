package connpool

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

// Regression tests: each one pins something that went wrong in real use.

// The error texts below are the ones seen in the logs of 2026-10-26/27 and in
// earlier incidents. Which of them may be sent again decides whether a user
// sees a 502 or nothing at all.
func TestRetryableClassification(t *testing.T) {
	post := func(ctx context.Context) *http.Request {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/x", bytes.NewReader([]byte("x")))
		return req
	}
	retried := []string{
		// Windows closing a connection under us (Proxifier, antivirus): 26 requests in one night.
		"write tcp 127.0.0.1:59942->8.6.112.5:443: wsasend: An established connection was aborted by the software in your host machine.",
		"read tcp 127.0.0.1:61531->8.6.112.0:443: wsarecv: An established connection was aborted by the software in your host machine.",
		"unexpected EOF",
		"EOF",
		"read tcp 10.0.0.2:5->1.1.1.1:443: read: connection reset by peer",
		"wsarecv: An existing connection was forcibly closed by the remote host.",
		"write tcp: broken pipe",
		"http2: server sent GOAWAY and closed the connection; LastStreamID=7, ErrCode=NO_ERROR, debug=\"\"",
		"http2: client conn is closed",
		"http2: client conn not usable",
		"use of closed network connection",
		"http: server closed idle connection",
		"net/http: HTTP/1.x transport connection broken: unexpected EOF",
		"connect: TLS handshake timeout",
		"dial tcp 8.6.112.0:443: connectex: A socket operation was attempted to an unreachable network.",
	}
	for _, msg := range retried {
		err := errors.New(msg)
		// The connection errors above that are timeouts need the Timeout() method.
		if strings.Contains(msg, "TLS handshake timeout") {
			err = timeoutErr{msg}
		}
		if strings.Contains(msg, "unreachable network") {
			err = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New(msg)}
		}
		if !retryable(post(context.Background()), err) {
			t.Errorf("not retried: %q", msg)
		}
	}

	notRetried := []error{
		timeoutErr{"net/http: timeout awaiting response headers"}, // a stalled backend: fail fast
		errors.New("x509: certificate signed by unknown authority"),
		errors.New("tls: failed to verify certificate: x509: certificate has expired"),
		errors.New("net/http: request canceled"),
		errors.New("proxy 127.0.0.1:1080: socks5: user name or password rejected"),
	}
	for _, err := range notRetried {
		if retryable(post(context.Background()), err) {
			t.Errorf("retried: %v", err)
		}
	}

	// A canceled request is never sent again, whatever the error says.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if retryable(post(ctx), errors.New("unexpected EOF")) {
		t.Error("a canceled request was retried")
	}
	// Neither is one whose body cannot be replayed: it could be sent twice.
	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/x", struct{ io.Reader }{strings.NewReader("x")})
	if req.GetBody != nil {
		t.Fatal("test setup: the body is replayable")
	}
	if retryable(req, errors.New("unexpected EOF")) {
		t.Error("a request with a body that cannot be replayed was retried")
	}
}

func TestConnectFailureClassification(t *testing.T) {
	for _, err := range []error{
		timeoutErr{"net/http: TLS handshake timeout"},
		&connectError{errors.New("TLS handshake timeout")},
		&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")},
		errors.New("proxyconnect tcp: dial tcp 127.0.0.1:1080: connect: connection refused"),
	} {
		if !connectFailure(err) {
			t.Errorf("%v is a connect failure (the request was never sent)", err)
		}
	}
	for _, err := range []error{
		errors.New("unexpected EOF"),
		errors.New("write tcp: wsasend: An established connection was aborted by the software in your host machine."),
		timeoutErr{"net/http: timeout awaiting response headers"},
	} {
		if connectFailure(err) {
			t.Errorf("%v is not a connect failure (the request may have been sent)", err)
		}
	}
}

// A body that cannot be replayed must reach the server once, whatever happens.
func TestNonReplayableBodyIsNeverResent(t *testing.T) {
	shortRetries(t, time.Minute)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		c, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = c.Close()
		}
	}))
	defer srv.Close()
	tr := New(0, 1, 1, nil)
	req, _ := http.NewRequest(http.MethodPost, srv.URL, struct{ io.Reader }{strings.NewReader("payload")})
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("no error from a server that drops the connection")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the server saw %d requests, want 1", n)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if n := tr.lanes[0].load(); n != 0 {
		t.Errorf("inflight = %d after the failed request", n)
	}
}

// Many requests at once while the lane limits change and the pools are renewed
// all the time: nothing may deadlock, leak a slot or fail. CI runs it with -race.
func TestConcurrentRequestsWhileLimitsAndPoolsChange(t *testing.T) {
	srv := okServer(t)
	tr := New(15*time.Millisecond, 3, 5, nil) // pools renew every 15 ms
	client := &http.Client{Transport: tr, Timeout: 20 * time.Second}

	stop := make(chan struct{})
	var changer sync.WaitGroup
	changer.Add(1)
	go func() {
		defer changer.Done()
		limits := [][2]int{{2, 4}, {5, 6}, {1, 1}, {4, 8}, {3, 5}}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				l := limits[i%len(limits)]
				tr.SetLimits(l[0], l[1])
			}
		}
	}()

	var wg sync.WaitGroup
	var failed atomic.Int32
	for g := 0; g < 24; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				resp, err := client.Post(srv.URL, "text/plain", bytes.NewReader([]byte("x")))
				if err != nil {
					failed.Add(1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	close(stop)
	changer.Wait()

	if n := failed.Load(); n != 0 {
		t.Errorf("%d of %d requests failed", n, 24*15)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	total := 0
	for _, l := range tr.lanes {
		total += l.load()
	}
	for _, d := range tr.draining {
		total += d.inflight
	}
	if total != 0 {
		t.Errorf("%d request slots leaked", total)
	}
}

// Requests for different ways out run side by side without mixing their pools.
func TestRouterConcurrentWaysOut(t *testing.T) {
	srv := okServer(t)
	proxyA, tunnelsA := connectProxy(t)
	proxyB, tunnelsB := connectProxy(t)
	r := NewRouter(time.Hour, 2, 3, nil)
	client := &http.Client{Transport: r, Timeout: 20 * time.Second}
	choices := []proxydial.Choice{{URL: proxyA}, {URL: proxyB}, {Direct: true}}

	var wg sync.WaitGroup
	var failed atomic.Int32
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := proxydial.WithChoice(context.Background(), choices[i%len(choices)])
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader([]byte("x")))
			resp, err := client.Do(req)
			if err != nil {
				failed.Add(1)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	if n := failed.Load(); n != 0 {
		t.Fatalf("%d requests failed", n)
	}
	if a, b := tunnelsA.Load(), tunnelsB.Load(); a == 0 || b == 0 || a > 10 || b > 10 {
		t.Errorf("tunnels A %d, B %d: each proxy carries its 10 requests, and only those", a, b)
	}
	if n := len(r.ts); n != 3 {
		t.Errorf("transports = %d, want one per way out", n)
	}
}

// A Router built without a transport factory builds pools like New does; Pool
// used to dereference the missing factory.
func TestRouterWithoutFactory(t *testing.T) {
	r := NewRouter(0, 0, 0, nil)
	if p := r.Pool(); p == nil || p.Proxy == nil {
		t.Errorf("Pool() = %+v, want a clone of the default transport", p)
	}
	if a, n := r.Limits(); a != DefaultActive || n != DefaultTotal {
		t.Errorf("limits = %d/%d", a, n)
	}
}
