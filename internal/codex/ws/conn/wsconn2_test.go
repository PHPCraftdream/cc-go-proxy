package conn

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
	"github.com/coder/websocket"
)

func TestWSKeepalivePongDataAndTimeout(t *testing.T) {
	oldPing, oldPong := wsPingAfter, wsPongTimeout
	wsPingAfter, wsPongTimeout = 25*time.Millisecond, 120*time.Millisecond
	t.Cleanup(func() { wsPingAfter, wsPongTimeout = oldPing, oldPong })
	for _, mode := range []string{"pong", "data", "lost"} {
		t.Run(mode, func(t *testing.T) {
			pinged := make(chan struct{}, 4)
			dataSent := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("OpenAI-Beta"); got != "responses_websockets=2026-02-06" {
					t.Errorf("OpenAI-Beta = %q", got)
				}
				ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
					InsecureSkipVerify: true,
					OnPingReceived: func(context.Context, []byte) bool {
						select {
						case pinged <- struct{}{}:
						default:
						}
						return mode == "pong"
					},
				})
				if err != nil {
					t.Errorf("accept: %v", err)
					return
				}
				defer ws.Close(websocket.StatusNormalClosure, "")
				_ = readWSRequest(t, ws)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if mode == "data" {
					go func() {
						for {
							if _, _, err := ws.Read(ctx); err != nil {
								return
							}
						}
					}()
					select {
					case <-pinged:
					case <-ctx.Done():
						t.Error("initial ping not received")
						return
					}
					for i := 0; i < 6; i++ {
						time.Sleep(100 * time.Millisecond)
						if err := ws.Write(ctx, websocket.MessageText, []byte(`{"type":"codex.rate_limits"}`)); err != nil {
							t.Errorf("write activity DATA: %v", err)
							return
						}
					}
					dataSent <- struct{}{}
					<-ctx.Done()
					return
				}
				for {
					if _, _, err := ws.Read(ctx); err != nil {
						return
					}
				}
			}))
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &codex.Request{Model: "m"}, nil)
			resp, err := conn.Do(ctx, []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "lost":
				select {
				case <-pinged:
				case <-time.After(300 * time.Millisecond):
					t.Fatal("keepalive ping not observed")
				}
				_, err = io.Copy(io.Discard, resp.Body)
				if err == nil || !strings.Contains(err.Error(), "client connection lost") {
					t.Fatalf("timeout body error = %v", err)
				}
				if conn.Err() == nil || !strings.Contains(conn.Err().Error(), "client connection lost") {
					t.Fatalf("Err = %v", conn.Err())
				}
			case "data":
				select {
				case <-dataSent:
				case <-time.After(2 * time.Second):
					t.Fatal("server did not send DATA")
				}
				if conn.Err() != nil {
					t.Fatalf("DATA should keep the connection alive: %v", conn.Err())
				}
				_, err = io.Copy(io.Discard, resp.Body)
				if err == nil || !strings.Contains(err.Error(), "client connection lost") {
					t.Fatalf("post-DATA quiet timeout = %v", err)
				}
			case "pong":
				select {
				case <-pinged:
				case <-time.After(300 * time.Millisecond):
					t.Fatal("keepalive ping not observed")
				}
				time.Sleep(180 * time.Millisecond)
				if conn.Err() != nil {
					t.Fatalf("pong did not keep connection alive: %v", conn.Err())
				}
				_ = resp.Body.Close()
			}
		})
	}
}

func TestWSCancelUnconsumedBody(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws := wsAccept(t, w, r)
		if ws == nil {
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		_ = readWSRequest(t, ws)
		writeWSEvent(t, ws, `{"type":"response.created","response":{"id":"cut-response"}}`)
		writeWSEvent(t, ws, `{"type":"response.output_item.done","item":{"id":"cut-item"}}`)
		started <- struct{}{}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _, _ = ws.Read(ctx)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &codex.Request{Model: "m"}, nil)
	resp, err := conn.Do(ctx, []byte(`{}`))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	<-started
	deadline := time.Now().Add(time.Second)
	for {
		result := conn.Result()
		if result.ResponseID == "cut-response" && len(result.Items) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pump did not record cut state: %+v", result)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	deadline = time.Now().Add(time.Second)
	for conn.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if conn.Err() == nil {
		t.Fatal("cancel did not close an unread stream")
	}
	if result := conn.Result(); result.Completed || result.ResponseID != "cut-response" || len(result.Items) != 1 {
		t.Fatalf("cut result = %+v", result)
	}
	_ = resp.Body.Close()
}

func TestWSFailureAndIncompleteRemainIncomplete(t *testing.T) {
	for _, event := range []string{
		`{"type":"response.failed","response":{"id":"failed"}}`,
		`{"type":"response.incomplete","response":{"id":"partial"}}`,
		`{"type":"error","message":"failure"}`,
	} {
		t.Run(strings.Trim(strings.Split(event, `"`)[3], "_"), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws := wsAccept(t, w, r)
				if ws == nil {
					return
				}
				defer ws.Close(websocket.StatusNormalClosure, "")
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_, _, err := ws.Read(ctx)
				if err != nil {
					t.Errorf("first request: %v", err)
					return
				}
				if err := ws.Write(ctx, websocket.MessageText, []byte(event)); err != nil {
					t.Errorf("first terminal: %v", err)
					return
				}
				_, _, err = ws.Read(ctx)
				if err != nil {
					t.Errorf("reused request: %v", err)
					return
				}
				if err := ws.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"next"}}`)); err != nil {
					t.Errorf("second response: %v", err)
					return
				}
				_, _, _ = ws.Read(ctx)
			}))
			defer server.Close()
			conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &codex.Request{Model: "m"}, nil)
			resp, err := conn.Do(context.Background(), []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if conn.Result().Completed {
				t.Fatal("non-completed terminal marked complete")
			}
			if conn.Err() != nil {
				t.Fatalf("terminal closed reusable connection: %v", conn.Err())
			}
			next, err := conn.Do(context.Background(), []byte(`{"next":true}`))
			if err != nil {
				t.Fatalf("connection not reusable after terminal event: %v", err)
			}
			if _, err := io.ReadAll(next.Body); err != nil {
				t.Fatal(err)
			}
			_ = next.Body.Close()
			if !conn.Result().Completed {
				t.Fatal("subsequent completed event not recorded")
			}
		})
	}
}

// A server that never answers the handshake is given up on by the handshake
// deadline alone: it is held until the test lets it go, so no wall-clock
// threshold decides the outcome, and only the deadline can end the dial.
func TestWSHandshakeDeadlineCanShrink(t *testing.T) {
	old := wsHandshakeTimeout
	wsHandshakeTimeout = 40 * time.Millisecond
	t.Cleanup(func() { wsHandshakeTimeout = old })
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no hijacker")
			return
		}
		c, _, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		<-release
	}))
	defer server.Close()
	defer close(release) // runs before server.Close, which waits for the handler
	session := testSession(t)
	done := make(chan error, 1)
	go func() {
		_, err := DialWS(context.Background(), server.URL, session, WSHandshake{Request: &codex.Request{Model: "m"}})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled handshake succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handshake deadline did not bound dial: a silent server was never given up on")
	}
}

func TestWSConfiguredProxyConnect(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("OpenAI-Beta"); got != "responses_websockets=2026-02-06" {
			t.Errorf("beta = %q", got)
		}
		ws := wsAccept(t, w, r)
		if ws == nil {
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		_ = readWSRequest(t, ws)
		writeWSEvent(t, ws, `{"type":"response.completed","response":{"id":"proxy"}}`)
	}))
	defer origin.Close()
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		dst, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, "dial failed", 502)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			dst.Close()
			return
		}
		client, rw, err := hj.Hijack()
		if err != nil {
			dst.Close()
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		go func() { _, _ = io.Copy(dst, rw); _ = dst.Close() }()
		_, _ = io.Copy(client, dst)
		_ = client.Close()
	}))
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := proxydial.WithChoice(context.Background(), proxydial.Choice{URL: proxyURL})
	conn := dialWSTest(t, ctx, origin.URL, testSession(t), &codex.Request{Model: "m"}, nil)
	resp, err := conn.Do(ctx, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := proxied.Load(); got != 1 {
		t.Fatalf("CONNECT proxy received %d requests, want one", got)
	}
}

func TestWSGoroutinesExitAfterCancelAndIdleClose(t *testing.T) {
	wsPingAfterOrig := wsPingAfter
	wsPingAfter = time.Hour
	t.Cleanup(func() { wsPingAfter = wsPingAfterOrig })
	connections := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		ws := wsAccept(t, w, r)
		if ws == nil {
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		typ, request, err := ws.Read(ctx)
		if err != nil || typ != websocket.MessageText {
			t.Errorf("read request: %v", err)
			return
		}
		if string(request) == `{"terminal":true}` {
			writeWSEvent(t, ws, `{"type":"response.completed","response":{"id":"idle-close"}}`)
		}
		_, _, _ = ws.Read(ctx)
	}))
	defer server.Close()
	baseline := runtime.NumGoroutine()
	for i := 0; i < 12; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &codex.Request{Model: "m"}, nil)
		resp, err := conn.Do(ctx, []byte(`{}`))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		_ = conn.Close()
	}
	terminalConn := dialWSTest(t, context.Background(), server.URL, testSession(t), &codex.Request{Model: "m"}, nil)
	terminal, err := terminalConn.Do(context.Background(), []byte(`{"terminal":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(terminal.Body); err != nil {
		t.Fatal(err)
	}
	_ = terminal.Body.Close()
	_ = terminalConn.Close()
	if connections.Load() != 13 {
		t.Fatalf("server connections = %d, want 13", connections.Load())
	}
	if got := connections.Load(); got != 13 {
		t.Fatalf("server saw %d websocket connections, want 13", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+1 {
		t.Fatalf("goroutines grew from %d to %d", baseline, got)
	}
}

func TestWSCloseReasonIsSanitized(t *testing.T) {
	const secret = "tok opaque-turn-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws := wsAccept(t, w, r)
		if ws == nil {
			return
		}
		defer ws.Close(websocket.StatusPolicyViolation, secret)
		_ = readWSRequest(t, ws)
	}))
	defer server.Close()
	conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &codex.Request{Model: "m"}, nil)
	resp, err := conn.Do(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_, bodyErr := io.ReadAll(resp.Body)
	if bodyErr == nil {
		t.Fatal("expected close error")
	}
	_ = resp.Body.Close()
	for _, err := range []error{bodyErr, conn.Err()} {
		if err == nil {
			t.Fatal("missing connection error")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("close reason leaked: %v", err)
		}
	}
}

func TestWSDirectAndEnvironmentProxyChoices(t *testing.T) {
	zeroTransport := newWSTransport(context.Background())
	defer zeroTransport.CloseIdleConnections()
	if zeroTransport.Proxy == nil {
		t.Fatal("zero choice lost net/http environment proxy selection")
	}
	tr := newWSTransport(proxydial.WithChoice(context.Background(), proxydial.Choice{Direct: true}))
	defer tr.CloseIdleConnections()
	if tr.Proxy != nil {
		t.Fatal("direct choice did not disable environment proxy")
	}
	if got := proxydial.ChoiceFor(context.Background()).Key(); got == "direct" {
		t.Fatal("zero choice unexpectedly became direct")
	}
}

// The websocket handshake presents the configured client version like inference.
func TestWSHandshakeUserAgent(t *testing.T) {
	tok := &oauth.Tokens{AccessToken: "t"}
	for _, tt := range []struct{ version, want string }{{"0.170.0", "codex_cli_rs/0.170.0 (ccgp)"}, {"", "codex_cli_rs (ccgp)"}} {
		ctx := context.Background()
		if tt.version != "" {
			ctx = codex.WithClientVersion(ctx, tt.version)
		}
		if got := wsHandshakeHeaders(ctx, WSHandshake{Request: &codex.Request{}}, tok).Get("User-Agent"); got != tt.want {
			t.Errorf("websocket user agent = %q, want %q", got, tt.want)
		}
	}
}
