package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/gofiber/fiber/v2"
)

// tcpPair is a connected pair of loopback TCP connections: server and client side.
func tcpPair(t *testing.T) (server, client net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	return server, client
}

func TestWatchClientGoneTellsAClosedClient(t *testing.T) {
	server, client := tcpPair(t)
	gone := make(chan struct{}, 1)
	stop := watchClientGone(server, func() { gone <- struct{}{} })
	defer stop()
	_ = client.Close()
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the closed client was not told")
	}
}

func TestWatchClientGoneStopsQuietlyAndLeavesTheConnectionUsable(t *testing.T) {
	server, client := tcpPair(t)
	var calls atomic.Int32
	stop := watchClientGone(server, func() { calls.Add(1) })
	time.Sleep(2 * clientWatchTick) // a few idle rounds
	started := time.Now()
	stop()
	stop() // twice is harmless
	if took := time.Since(started); took > 2*clientWatchTick {
		t.Errorf("stop took %s", took)
	}
	if calls.Load() != 0 {
		t.Fatal("a client that stayed was taken for gone")
	}
	// No deadline is left behind: the connection still reads and writes.
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := io.ReadFull(server, b[:]); err != nil || b[0] != 'x' {
		t.Fatalf("read after the watch: %q, %v", b[:], err)
	}
	_ = client.Close()
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 0 {
		t.Error("the watch went on after stop")
	}
}

func TestWatchClientGoneIgnoresAConnectionThatIsNotTCP(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	var calls atomic.Int32
	stop := watchClientGone(a, func() { calls.Add(1) })
	_ = b.Close()
	time.Sleep(50 * time.Millisecond)
	stop()
	if calls.Load() != 0 {
		t.Error("an in-memory connection was watched")
	}
}

// A request without streaming sends nothing until its reply is complete; when
// its client goes away meanwhile, the upstream call is canceled and the log says so.
func TestNonStreamReplyIsCanceledWhenTheClientGoesAway(t *testing.T) {
	received := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"))
		w.(http.Flusher).Flush()
		received <- struct{}{}
		select {
		case <-r.Context().Done():
			canceled <- struct{}{}
		case <-time.After(20 * time.Second):
		}
	}))
	defer backend.Close()

	oldClient, oldLog := codex.HTTPClient, logOut
	t.Cleanup(func() { codex.HTTPClient, logOut = oldClient, oldLog })
	codex.HTTPClient = &http.Client{}
	var sl, al syncBuf
	logOut = &sl
	store := oauth.Store{Path: filepath.Join(t.TempDir(), "o.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: "tok", RefreshToken: "rt", AccountID: "acc", LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	sess := &oauth.Session{Store: store, Client: &oauth.Client{}}
	cfg := &config.Config{
		Profile: "codex", ProviderName: "codex", CodexTransport: "http",
		Providers: map[string]*config.Provider{"codex": {Name: "codex", Type: "codex", BaseURL: backend.URL}},
	}
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(accessLog(&al))
	app.Post("/call/:token/v1/messages", func(c *fiber.Ctx) error { return handleCodexMessages(c, cfg, sess) })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = app.Listener(ln) }()
	defer func() { _ = app.ShutdownWithTimeout(5 * time.Second) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"claude-opus-5-5","max_tokens":10,"stream":false,"messages":[{"role":"user","content":"hi"}]}`
	_, _ = fmt.Fprintf(conn, "POST /call/secret/v1/messages HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	select {
	case <-received:
	case <-time.After(10 * time.Second):
		t.Fatal("the backend got no request")
	}
	_ = conn.Close()
	select {
	case <-canceled:
	case <-time.After(10 * time.Second):
		t.Fatal("the upstream call went on after the client left")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(sl.String(), "non-stream reply canceled by client after") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(sl.String(), "non-stream reply canceled by client after") {
		t.Errorf("log lacks the canceled line: %q", sl.String())
	}
	for !strings.Contains(al.String(), "POST") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(al.String(), "| non-stream") {
		t.Errorf("access line lacks the non-stream note: %q", al.String())
	}
}

// A client that stays gets its reply on a connection closed after it.
func TestNonStreamReplyClosesTheConnection(t *testing.T) {
	nonStreamBody(t)
	status, body, access, _ := codexCall(t, overloadBackend(new(atomic.Int32), 0), time.Minute)
	if status != fiber.StatusOK || !strings.Contains(body, `"hi"`) {
		t.Fatalf("status %d, body %q", status, body)
	}
	if !strings.Contains(access, "| non-stream") {
		t.Errorf("access line lacks the non-stream note: %q", access)
	}
	if !lastCodexClose {
		t.Error("the reply did not close its connection")
	}
}

// Data from the client is not a close: the watch ends without a word, and a
// close after it is not taken for the client going away.
func TestWatchClientGoneEndsOnData(t *testing.T) {
	server, client := tcpPair(t)
	var calls atomic.Int32
	stop := watchClientGone(server, func() { calls.Add(1) })
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * clientWatchTick)
	_ = client.Close()
	time.Sleep(2 * clientWatchTick)
	started := time.Now()
	stop()
	if took := time.Since(started); took > clientWatchTick {
		t.Errorf("stop after the watch ended took %s", took)
	}
	if calls.Load() != 0 {
		t.Error("a client that sent data was taken for gone")
	}
}

// The end lines of a reply without streaming say so.
func TestUpstreamEndNamesANonStreamReply(t *testing.T) {
	var buf bytes.Buffer
	old := logOut
	logOut = &buf
	t.Cleanup(func() { logOut = old })
	for _, nonStream := range []bool{false, true} {
		buf.Reset()
		trace := newUpstreamTrace()
		trace.nonStream = nonStream
		trace.start = time.Now().Add(-slowTotal - time.Second)
		logUpstreamEnd(context.Background(), &codex.Translator{}, trace, &http.Response{Header: http.Header{}}, "codex", "a -> b", 10)
		want := "] slow stream "
		if nonStream {
			want = "] slow non-stream reply "
		}
		if !strings.Contains(buf.String(), want) {
			t.Errorf("nonStream %v: line %q, want %q", nonStream, buf.String(), want)
		}
	}
}
