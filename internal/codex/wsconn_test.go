package codex

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
	"github.com/coder/websocket"
)

func dialWSTest(t *testing.T, ctx context.Context, serverURL string, session *oauth.Session, request *Request, routing *Routing) *WSConn {
	t.Helper()
	conn, err := DialWS(ctx, serverURL, session, WSHandshake{Request: request, Routing: routing})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func wsAccept(t *testing.T, w http.ResponseWriter, r *http.Request) *websocket.Conn {
	t.Helper()
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		t.Errorf("accept websocket: %v", err)
		return nil
	}
	return conn
}

func readWSRequest(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Errorf("read websocket request: %v", err)
		return nil
	}
	if typ != websocket.MessageText {
		t.Errorf("request frame type = %v, want text", typ)
	}
	return data
}

func writeWSEvent(t *testing.T, conn *websocket.Conn, event string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(event)); err != nil {
		t.Errorf("write event: %v", err)
	}
}

func TestWSHandshakeHeadersAndRouting(t *testing.T) {
	const turn = "opaque-turn-state"
	routing := NewRouting("thread-ws", turn, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/responses" {
			t.Errorf("websocket path = %q", got)
		}
		if got := r.Header.Get("Sec-WebSocket-Extensions"); got != "" {
			t.Errorf("compression was offered: %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acc" {
			t.Errorf("account = %q", got)
		}
		if got := r.Header.Get("originator"); got != "codex_cli_rs" {
			t.Errorf("originator = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "codex_cli_rs (ccgp)" {
			t.Errorf("user agent = %q", got)
		}
		if got := r.Header.Get("session-id"); got != "session-key" {
			t.Errorf("session = %q", got)
		}
		if got := r.Header.Get("x-codex-routing-hint"); got != "model=gpt-test" {
			t.Errorf("routing hint = %q", got)
		}
		if got := r.Header.Get("thread-id"); got != "thread-ws" {
			t.Errorf("thread-id = %q", got)
		}
		if got := r.Header.Get("x-client-request-id"); got != "thread-ws" {
			t.Errorf("request id = %q", got)
		}
		if got := r.Header.Get("x-codex-turn-state"); got != turn {
			t.Errorf("turn state = %q", got)
		}
		if got := r.Header.Get("OpenAI-Beta"); got != "responses_websockets=2026-02-06" {
			t.Errorf("OpenAI-Beta = %q", got)
		}
		w.Header().Set("x-codex-turn-state", "returned-turn")
		ws := wsAccept(t, w, r)
		if ws == nil {
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		_ = readWSRequest(t, ws)
		writeWSEvent(t, ws, `{"type":"response.completed","response":{"id":"resp-one"}}`)
	}))
	defer server.Close()

	conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &Request{Model: "gpt-test", PromptCacheKey: "session-key"}, routing)
	resp, err := conn.Do(context.Background(), []byte(`{"input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !routing.Returned() {
		t.Fatal("routing response header was not captured")
	}
	if _, state := routing.outgoing(); state != "returned-turn" {
		t.Fatalf("captured turn state = %q, want the one the handshake returned", state)
	}
}

func TestWSSSEFramesAndTranslator(t *testing.T) {
	frames := []string{
		`{"type":"codex.rate_limits","limit":17}`,
		`{"type":"response.created","response":{"id":"resp-stream"}}`,
		`{"type":"response.output_item.added","item":{"id":"msg-1","type":"message"}}`,
		`{"type":"response.output_text.delta","item_id":"msg-1","delta":"hello"}`,
		`{"type":"response.output_item.done","item":{"id":"msg-1","type":"function_call","call_id":"call-1","name":"tool","arguments":"{}"}}`,
		`{"type":"response.completed","response":{"id":"resp-stream","usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":3},"output_tokens":4,"output_tokens_details":{"reasoning_tokens":2}}}}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("OpenAI-Beta") != "responses_websockets=2026-02-06" {
			t.Errorf("missing Beta handshake header: %q", r.Header.Get("OpenAI-Beta"))
		}
		ws := wsAccept(t, w, r)
		if ws == nil {
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		typ, request, err := ws.Read(ctx)
		if err != nil || typ != websocket.MessageText {
			t.Errorf("read request: type=%v err=%v", typ, err)
			return
		}
		if string(request) != `{"stream":true}` {
			t.Errorf("request frame = %s", request)
		}
		for _, frame := range frames {
			if err := ws.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
				t.Errorf("write frame: %v", err)
				return
			}
		}
	}))
	defer server.Close()
	conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &Request{Model: "m"}, nil)
	resp, err := conn.Do(context.Background(), []byte(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Connection") != "Upgrade" || resp.Header.Get("Upgrade") != "websocket" || resp.Header.Get("Sec-WebSocket-Accept") == "" {
		t.Fatalf("handshake response headers not cloned: %+v", resp.Header)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	var want strings.Builder
	for _, frame := range frames {
		fmt.Fprintf(&want, "data: %s\n\n", frame)
	}
	if string(got) != want.String() {
		t.Fatalf("SSE bytes differ\n got: %q\nwant: %q", got, want.String())
	}
	var events []string
	var messageDelta, textDelta []byte
	translator := &Translator{Model: "m", Emit: func(name string, data any) {
		events = append(events, name)
		if name == "message_delta" {
			messageDelta, _ = json.Marshal(data)
		}
		if name == "content_block_delta" {
			textDelta, _ = json.Marshal(data)
		}
	}}
	if err := translator.Run(bytes.NewReader(got)); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "message_start,content_block_start,content_block_delta,content_block_stop,message_delta,message_stop" {
		t.Fatalf("translated events = %v", events)
	}
	if translator.Usage != (Usage{Input: 12, Cached: 3, Output: 4, Reasoning: 2}) {
		t.Fatalf("usage = %+v", translator.Usage)
	}
	if !bytes.Contains(textDelta, []byte(`"text":"hello"`)) {
		t.Fatalf("text delta missing: %s", textDelta)
	}
	for _, expected := range []string{`"input_tokens":9`, `"cache_read_input_tokens":3`, `"output_tokens":4`} {
		if !bytes.Contains(messageDelta, []byte(expected)) {
			t.Errorf("message_delta missing %s: %s", expected, messageDelta)
		}
	}
}

func TestWSTerminalFramesAllowImmediateReuse(t *testing.T) {
	frames := []string{
		`{"type":"response.completed","response":{"id":"completed"}}`,
		`{"type":"response.failed","response":{"id":"failed"}}`,
		`{"type":"response.incomplete","response":{"id":"incomplete"}}`,
		`{"type":"error","message":"safe test error"}`,
	}
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws := wsAccept(t, w, r)
		if ws == nil {
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		for range len(frames) {
			typ, _, err := ws.Read(ctx)
			if err != nil {
				t.Errorf("read request: %v", err)
				return
			}
			if typ != websocket.MessageText {
				t.Errorf("request frame type = %v", typ)
				return
			}
			i := int(received.Add(1) - 1)
			if err := ws.Write(ctx, websocket.MessageText, []byte(frames[i])); err != nil {
				t.Errorf("write terminal frame: %v", err)
				return
			}
		}
		_, _, _ = ws.Read(ctx)
	}))
	defer server.Close()
	conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &Request{Model: "m"}, nil)
	for i, frame := range frames {
		resp, err := conn.Do(context.Background(), []byte(fmt.Sprintf(`{"n":%d}`, i)))
		if err != nil {
			t.Fatalf("Do %d: %v", i, err)
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("terminal %d body: %v", i, err)
		}
		_ = resp.Body.Close()
		if !bytes.Contains(data, []byte("data: "+frame+"\n\n")) {
			t.Fatalf("terminal frame %d absent: %q", i, data)
		}
		wantCompleted := i == 0
		if got := conn.Result().Completed; got != wantCompleted {
			t.Fatalf("Completed after %s = %v, want %v", frame, got, wantCompleted)
		}
		if conn.Err() != nil {
			t.Fatalf("connection dead after terminal %s: %v", frame, conn.Err())
		}
	}
}

func TestWSResultItemsAreDeepCopied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws := wsAccept(t, w, r)
		if ws == nil {
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		_ = readWSRequest(t, ws)
		writeWSEvent(t, ws, `{"type":"response.created","response":{"id":"resp-copy"}}`)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		received := []byte(`{"type":"response.output_item.done","item":{"id":"item-1","type":"message"}}`)
		if err := ws.Write(ctx, websocket.MessageText, received); err != nil {
			t.Errorf("write item: %v", err)
		}
		writeWSEvent(t, ws, `{"type":"response.completed","response":{"id":"resp-copy"}}`)
	}))
	defer server.Close()
	conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &Request{Model: "m"}, nil)
	resp, err := conn.Do(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	result := conn.Result()
	if result.ResponseID != "resp-copy" || !result.Completed || len(result.Items) != 1 {
		t.Fatalf("canonical response.output_item.done not captured: %+v", result)
	}
	if got := string(result.Items[0]); got != `{"id":"item-1","type":"message"}` {
		t.Fatalf("result item = %s", got)
	}
	result.Items[0][0] = 'x'
	if got := string(conn.Result().Items[0]); got != `{"id":"item-1","type":"message"}` {
		t.Fatalf("result item mutated through snapshot: %s", got)
	}
}

func TestWSReadLimitOver16MiB(t *testing.T) {
	oldPing, oldPong := wsPingAfter, wsPongTimeout
	wsPingAfter, wsPongTimeout = time.Hour, time.Hour
	t.Cleanup(func() { wsPingAfter, wsPongTimeout = oldPing, oldPong })
	payload := []byte(`{"type":"response.output_text.delta","delta":"` + strings.Repeat("x", 17<<20) + `"}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws := wsAccept(t, w, r)
		if ws == nil {
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		_ = readWSRequest(t, ws)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := ws.Write(ctx, websocket.MessageText, payload); err != nil {
			t.Errorf("write large message: %v", err)
			return
		}
		if err := ws.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"large"}}`)); err != nil {
			t.Errorf("write complete event: %v", err)
			return
		}
		_, _, _ = ws.Read(ctx)
		_, _, _ = ws.Read(ctx)
	}))
	defer server.Close()
	conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &Request{Model: "m"}, nil)
	resp, err := conn.Do(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(data) < 17<<20 {
		t.Fatalf("received only %d bytes", len(data))
	}
	if !conn.Result().Completed {
		t.Fatalf("large frame exchange not completed: %+v", conn.Result())
	}
}

func TestWSBusyCancelAndEarlyClose(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws := wsAccept(t, w, r)
		if ws == nil {
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		_ = readWSRequest(t, ws)
		started <- struct{}{}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _, _ = ws.Read(ctx)
	}))
	defer server.Close()
	conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &Request{Model: "m"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := conn.Do(ctx, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := conn.Do(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("Do accepted second in-flight exchange")
	}
	cancel()
	_, readErr := io.ReadAll(resp.Body)
	if readErr == nil {
		t.Fatal("cancelled exchange ended without a body error")
	}
	_ = resp.Body.Close()
	if conn.Err() == nil {
		t.Fatal("cancel did not terminate connection")
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	conn2 := dialWSTest(t, ctx2, server.URL, testSession(t), &Request{Model: "m"}, nil)
	resp2, err := conn2.Do(ctx2, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := resp2.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if conn2.Err() == nil {
		t.Fatal("early body close did not terminate connection")
	}
	cancel2()
}

func TestWSExplicitCloseAndAge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws := wsAccept(t, w, r)
		if ws == nil {
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _, _ = ws.Read(ctx)
	}))
	defer server.Close()
	conn, err := DialWS(context.Background(), server.URL, testSession(t), WSHandshake{Request: &Request{Model: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if conn.Age() <= 0 {
		t.Fatalf("age = %s, want positive age", conn.Age())
	}
	if conn.Err() != nil {
		t.Fatalf("live connection Err = %v", conn.Err())
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if conn.Err() == nil {
		t.Fatal("Err is nil after explicit close")
	}
	if _, err := conn.Do(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("Do succeeded after Close")
	}
}

func TestWSTransportTLSHTTP1Only(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.ProtoMajor; got != 1 {
			t.Errorf("request HTTP major = %d", got)
		}
		if got := r.TLS.NegotiatedProtocol; got != "" && got != "http/1.1" {
			t.Errorf("ALPN = %q", got)
		}
		ws := wsAccept(t, w, r)
		if ws == nil {
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		_ = readWSRequest(t, ws)
		writeWSEvent(t, ws, `{"type":"response.completed","response":{"id":"tls"}}`)
	}))
	defer server.Close()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	defaultTransport := http.DefaultTransport.(*http.Transport)
	oldTLS := defaultTransport.TLSClientConfig
	cfg := &tls.Config{RootCAs: pool}
	defaultTransport.TLSClientConfig = cfg
	t.Cleanup(func() { defaultTransport.TLSClientConfig = oldTLS })
	clone := newWSTransport(context.Background())
	defer clone.CloseIdleConnections()
	if clone.TLSClientConfig == cfg || clone.TLSClientConfig.RootCAs != pool {
		t.Fatal("handshake transport did not retain cloned custom roots")
	}
	if got := clone.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("handshake ALPN = %v", got)
	}
	tr := newWSTransport(context.Background())
	defer tr.CloseIdleConnections()
	if tr.TLSClientConfig == cfg || tr.TLSClientConfig.RootCAs != pool {
		t.Fatal("websocket transport did not clone and preserve TLS roots")
	}
	if got := tr.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" || tr.ForceAttemptHTTP2 || len(tr.TLSNextProto) != 0 {
		t.Fatalf("transport permits HTTP/2: %+v", tr.TLSClientConfig.NextProtos)
	}
	conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &Request{Model: "m"}, nil)
	resp, err := conn.Do(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}

func TestWSHandshakeErrorsAreSanitized(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.Header().Set("X-Leak", "Bearer tok old-turn-secret new-turn-secret")
				w.Header().Set("x-codex-turn-state", "new-turn-secret")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "body tok old-turn-secret new-turn-secret")
			}))
			defer server.Close()
			_, err := DialWS(context.Background(), server.URL, testSession(t), WSHandshake{
				Request: &Request{Model: "m"}, Routing: NewRouting("thread-secret", "old-turn-secret", func(state string) {}),
			})
			var handshake *WSHandshakeError
			if !errors.As(err, &handshake) {
				t.Fatalf("error = %T %v", err, err)
			}
			if handshake.Status != status || handshake.Header.Get("Retry-After") != "7" {
				t.Fatalf("handshake = %+v", handshake)
			}
			formatted := err.Error() + " " + fmt.Sprint(errors.Unwrap(err)) + " " + fmt.Sprint(handshake.Header)
			for _, secret := range []string{"tok", "rt", "acc", "thread-secret", "old-turn-secret", "new-turn-secret", "body"} {
				if strings.Contains(formatted, secret) {
					t.Fatalf("%q leaked through handshake error: %s", secret, formatted)
				}
			}
			if handshake.Header.Get("Authorization") != "" || handshake.Header.Get("x-codex-turn-state") != "" || handshake.Header.Get("X-Leak") != "" {
				t.Fatalf("sensitive headers not removed: %+v", handshake.Header)
			}
			if handshake.Err == nil || errors.Unwrap(err) != handshake.Err {
				t.Fatal("Unwrap did not preserve sanitized Err")
			}
		})
	}
}

func TestWS401RefreshExactlyOnce(t *testing.T) {
	for _, cases := range []struct {
		name           string
		statuses       []int
		wantHandshakes int
		thread         string
		turn           string
		retryTurn      string
	}{
		{name: "refresh-success", statuses: []int{401, 101}, wantHandshakes: 2, thread: "thread-refresh", turn: "before-turn", retryTurn: "after-turn"},
		{name: "refresh-still-unauthorized", statuses: []int{401, 401}, wantHandshakes: 2, thread: "thread-refresh", turn: "before-turn", retryTurn: "before-turn"},
	} {
		t.Run(cases.name, func(t *testing.T) {
			handshakes := atomic.Int32{}
			refreshes := atomic.Int32{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				i := int(handshakes.Add(1) - 1)
				if i >= len(cases.statuses) {
					t.Errorf("unexpected handshake %d", i+1)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if cases.statuses[i] == http.StatusUnauthorized {
					if i == 0 && r.Header.Get("x-codex-turn-state") != "before-turn" {
						t.Errorf("first captured routing turn = %q", r.Header.Get("x-codex-turn-state"))
					}
					if i == 1 && r.Header.Get("x-codex-turn-state") != cases.turn {
						t.Errorf("retry on second 401 turn = %q", r.Header.Get("x-codex-turn-state"))
					}
					w.Header().Set("x-codex-turn-state", cases.turn)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if cases.statuses[i] == http.StatusSwitchingProtocols {
					if r.Header.Get("Authorization") != "Bearer new-token" {
						t.Errorf("retry authorization = %q", r.Header.Get("Authorization"))
					}
					if r.Header.Get("thread-id") != cases.thread {
						t.Errorf("thread id = %q", r.Header.Get("thread-id"))
					}
				}
				ws := wsAccept(t, w, r)
				if ws == nil {
					return
				}
				defer ws.Close(websocket.StatusNormalClosure, "")
				_ = readWSRequest(t, ws)
				writeWSEvent(t, ws, `{"type":"response.created","response":{"id":"ok"}}`)
				if cases.statuses[i] == http.StatusSwitchingProtocols {
					writeWSEvent(t, ws, `{"type":"response.output_item.done","item":{"id":"turn-state","type":"message"}}`)
				}
				writeWSEvent(t, ws, `{"type":"response.completed","response":{"id":"ok"}}`)
			}))
			defer server.Close()
			issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				refreshes.Add(1)
				_, _ = io.WriteString(w, `{"access_token":"new-token","refresh_token":"rt2"}`)
			}))
			defer issuer.Close()
			session := testSession(t)
			route := NewRouting(cases.thread, cases.turn, nil)
			session.Client = &oauth.Client{Issuer: issuer.URL}
			conn, err := DialWS(context.Background(), server.URL, session, WSHandshake{Request: &Request{Model: "m"}, Routing: route})
			if cases.statuses[len(cases.statuses)-1] == http.StatusUnauthorized {
				var handshake *WSHandshakeError
				if !errors.As(err, &handshake) || handshake.Status != http.StatusUnauthorized {
					t.Fatalf("expected second 401, got %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				resp, doErr := conn.Do(context.Background(), []byte(`{"stream":true}`))
				if doErr != nil {
					t.Fatal(doErr)
				}
				if _, doErr = io.Copy(io.Discard, resp.Body); doErr != nil {
					t.Fatal(doErr)
				}
				_ = resp.Body.Close()
				_ = conn.Close()
			}
			if got := int(handshakes.Load()); got != cases.wantHandshakes {
				t.Fatalf("handshakes = %d, want %d", got, cases.wantHandshakes)
			}
			if got := int(refreshes.Load()); got != 1 {
				t.Fatalf("token refreshes = %d, want exactly one", got)
			}
		})
	}
}

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
			conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &Request{Model: "m"}, nil)
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
	conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &Request{Model: "m"}, nil)
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
			conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &Request{Model: "m"}, nil)
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
func TestWSHandshakeDeadlineCanShrink(t *testing.T) {
	old := wsHandshakeTimeout
	wsHandshakeTimeout = 40 * time.Millisecond
	t.Cleanup(func() { wsHandshakeTimeout = old })
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
		<-time.After(250 * time.Millisecond)
	}))
	defer server.Close()
	start := time.Now()
	_, err := DialWS(context.Background(), server.URL, testSession(t), WSHandshake{Request: &Request{Model: "m"}})
	if err == nil {
		t.Fatal("stalled handshake succeeded")
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("handshake deadline did not bound dial: %s", time.Since(start))
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
	conn := dialWSTest(t, ctx, origin.URL, testSession(t), &Request{Model: "m"}, nil)
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
		conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &Request{Model: "m"}, nil)
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
	terminalConn := dialWSTest(t, context.Background(), server.URL, testSession(t), &Request{Model: "m"}, nil)
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
	conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &Request{Model: "m"}, nil)
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
