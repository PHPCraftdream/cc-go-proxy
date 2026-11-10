package codex

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
	"github.com/coder/websocket"
)

// These variables let tests shorten timing.
var (
	wsHandshakeTimeout       = 15 * time.Second
	wsPingAfter              = 15 * time.Second
	wsPongTimeout            = 30 * time.Second
	wsReadLimit        int64 = 32 << 20
	wsPing                   = func(conn *websocket.Conn, ctx context.Context) error { return conn.Ping(ctx) }
)

var (
	errWSBusy           = errors.New("codex websocket: another exchange is in progress")
	errWSClosed         = errors.New("codex websocket: connection closed")
	errWSClientLost     = errors.New("codex websocket: client connection lost")
	errWSConnection     = errors.New("codex websocket: connection failed")
	errWSRefresh        = errors.New("codex websocket: token refresh failed")
	errWSAuthentication = errors.New("codex websocket: authentication failed")
)

// WSHandshake carries the request and optional routing state.
type WSHandshake struct {
	Request *Request
	Routing *Routing
}

// WSResult is the accumulated summary of a websocket Responses exchange.
type WSResult struct {
	ResponseID string
	Completed  bool
	Items      []json.RawMessage
}

// WSHandshakeError reports an unsuccessful upgrade.
type WSHandshakeError struct {
	Status int
	Header http.Header
	Err    error
}

// Error never includes headers, tokens or the turn state.
func (e *WSHandshakeError) Error() string {
	if e == nil {
		return "codex websocket: handshake failed"
	}
	if e.Status != 0 {
		return fmt.Sprintf("codex websocket: handshake returned HTTP %d", e.Status)
	}
	return "codex websocket: handshake failed"
}

func (e *WSHandshakeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// WSConn is a single reusable Codex websocket connection.
type WSConn struct {
	mu        sync.Mutex
	conn      *websocket.Conn
	transport *http.Transport
	headers   http.Header
	started   time.Time
	lifeCtx   context.Context
	cancel    context.CancelFunc
	activity  *wsActivity

	closed bool
	busy   bool
	active *wsExchange
	err    error
	result WSResult
}

type wsExchange struct {
	body    *wsStream
	done    chan struct{}
	doneOne sync.Once
	stopCtx func() bool
}

func (e *wsExchange) finish() { e.doneOne.Do(func() { close(e.done) }) }

type wsActivity struct {
	last   atomic.Int64
	notify chan struct{}
}

func newWSActivity() *wsActivity {
	a := &wsActivity{notify: make(chan struct{}, 1)}
	a.touch()
	return a
}

func (a *wsActivity) touch() {
	a.last.Store(time.Now().UnixNano())
	select {
	case a.notify <- struct{}{}:
	default:
	}
}

func (a *wsActivity) since() time.Time { return time.Unix(0, a.last.Load()) }

// DialWS opens a persistent Responses websocket. Only the handshake is
// time-bounded; the resulting connection has no overall timeout.
func DialWS(ctx context.Context, baseURL string, s *oauth.Session, h WSHandshake) (*WSConn, error) {
	if ctx == nil {
		return nil, errors.New("codex websocket: invalid context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if h.Request == nil {
		return nil, errors.New("codex websocket: nil request")
	}
	if s == nil {
		return nil, errWSAuthentication
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	endpoint, err := wsEndpoint(baseURL)
	if err != nil {
		return nil, err
	}
	tok, err := s.Token(ctx)
	if err != nil {
		return nil, safeContextError(ctx, err, errWSAuthentication)
	}
	allSecrets := appendTokenSecrets(nil, tok)
	allSecrets = appendSecret(allSecrets, routingState(h.Routing)...)

	for attempt := 0; attempt < 2; attempt++ {
		tr := newWSTransport(ctx)
		client := &http.Client{
			Transport: tr,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		activity := newWSActivity()
		tr.TLSClientConfig = ensureWSALPN(tr.TLSClientConfig)
		handshakeCtx, cancel := context.WithTimeout(ctx, wsHandshakeTimeout)
		conn, resp, dialErr := websocket.Dial(handshakeCtx, endpoint, &websocket.DialOptions{
			HTTPClient:      client,
			HTTPHeader:      wsHandshakeHeaders(h, tok),
			CompressionMode: websocket.CompressionDisabled,
			OnPingReceived:  func(context.Context, []byte) bool { activity.touch(); return true },
			OnPongReceived: func(context.Context, []byte) {
				activity.touch()
			},
		})
		cancel()
		if resp != nil && h.Routing != nil {
			if _, oldState := h.Routing.outgoing(); oldState != "" {
				allSecrets = appendSecret(allSecrets, oldState)
			}
		}
		if resp != nil && h.Routing != nil {
			h.Routing.capture(resp.Header)
		}
		if resp != nil {
			allSecrets = appendSecret(allSecrets, routingState(h.Routing)...)
			if state, ok := headerValueFold(resp.Header, "x-codex-turn-state"); ok {
				allSecrets = appendSecret(allSecrets, state)
			}
		}
		if resp != nil && resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			closeHandshakeResponse(resp)
			tr.CloseIdleConnections()
			allSecrets = appendTokenSecrets(allSecrets, tok)
			tok, err = s.ForceRefresh(ctx, tok.AccessToken)
			if err != nil {
				return nil, safeContextError(ctx, err, errWSRefresh)
			}
			allSecrets = appendTokenSecrets(allSecrets, tok)
			continue
		}
		if dialErr != nil || resp == nil || resp.StatusCode != http.StatusSwitchingProtocols {
			status := 0
			var header http.Header
			if resp != nil {
				status = resp.StatusCode
				header = safeHandshakeHeader(resp.Header, allSecrets...)
				closeHandshakeResponse(resp)
			}
			tr.CloseIdleConnections()
			if resp != nil {
				return nil, &WSHandshakeError{
					Status: status, Header: header,
					Err: errors.New("codex websocket: handshake failed"),
				}
			}
			return nil, safeContextError(ctx, dialErr, errWSConnection)
		}

		conn.SetReadLimit(wsReadLimit)
		lifeCtx, lifeCancel := context.WithCancel(context.Background())
		c := &WSConn{
			conn: conn, transport: tr, headers: resp.Header.Clone(), started: time.Now(),
			lifeCtx: lifeCtx, cancel: lifeCancel, activity: activity,
		}
		go c.readPump()
		return c, nil
	}
	return nil, errWSAuthentication
}

func wsEndpoint(baseURL string) (string, error) {
	raw := strings.TrimRight(baseURL, "/") + "/responses"
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("codex websocket: invalid endpoint")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
	default:
		return "", errors.New("codex websocket: unsupported endpoint scheme")
	}
	if u.Host == "" {
		return "", errors.New("codex websocket: invalid endpoint")
	}
	return u.String(), nil
}

func newWSTransport(ctx context.Context) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	tr.TLSClientConfig = ensureWSALPN(tr.TLSClientConfig)
	proxydial.Apply(tr, proxydial.ChoiceFor(ctx))
	return tr
}

func ensureWSALPN(config *tls.Config) *tls.Config {
	if config == nil {
		config = &tls.Config{}
	} else {
		config = config.Clone()
	}
	config.NextProtos = []string{"http/1.1"}
	return config
}

func wsHandshakeHeaders(h WSHandshake, tok *oauth.Tokens) http.Header {
	headers := sessionHeaders(h.Request).Clone()
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	headers.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
	headers.Set("Authorization", "Bearer "+tok.AccessToken)
	if tok.AccountID != "" {
		headers.Set("ChatGPT-Account-ID", tok.AccountID)
	}
	headers.Set("originator", originator)
	headers.Set("User-Agent", originator+" (ccgp)")
	if h.Routing != nil {
		threadID, state := h.Routing.outgoing()
		headers.Set("thread-id", threadID)
		headers.Set("x-client-request-id", threadID)
		if state != "" {
			headers.Set("x-codex-turn-state", state)
		}
	}
	return headers
}

func routingState(r *Routing) []string {
	if r == nil {
		return nil
	}
	thread, state := r.outgoing()
	return []string{thread, state}
}

func appendTokenSecrets(secrets []string, tok *oauth.Tokens) []string {
	return appendSecret(secrets, tok.AccessToken, tok.RefreshToken, tok.IDToken, tok.AccountID)
}

func appendSecret(secrets []string, values ...string) []string {
	for _, value := range values {
		if value == "" {
			continue
		}
		found := false
		for _, secret := range secrets {
			if secret == value {
				found = true
				break
			}
		}
		if !found {
			secrets = append(secrets, value)
		}
	}
	return secrets
}

func safeHandshakeHeader(header http.Header, secrets ...string) http.Header {
	copy := header.Clone()
	for key, values := range copy {
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "Proxy-Authorization") ||
			strings.EqualFold(key, "x-codex-turn-state") || strings.EqualFold(key, "Cookie") || strings.EqualFold(key, "Set-Cookie") {
			delete(copy, key)
			continue
		}
		for _, value := range values {
			for _, secret := range secrets {
				if secret != "" && strings.Contains(value, secret) {
					delete(copy, key)
					break
				}
			}
		}
	}
	return copy
}

func closeHandshakeResponse(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}

func safeContextError(ctx context.Context, err, fallback error) error {
	if ctx != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return fallback
}

// Do sends frame (one response.create) and returns the reply as an SSE stream
// that ends after the terminal event; one exchange at a time.
func (c *WSConn) Do(ctx context.Context, frame []byte) (*http.Response, error) {
	if c == nil {
		return nil, errWSClosed
	}
	if ctx == nil {
		return nil, errors.New("codex websocket: invalid context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed {
		err := c.err
		if err == nil {
			err = errWSClosed
		}
		c.mu.Unlock()
		return nil, err
	}
	if c.busy {
		c.mu.Unlock()
		return nil, errWSBusy
	}
	c.busy = true
	c.result = WSResult{}
	ex := &wsExchange{body: newWSStream(), done: make(chan struct{})}
	ex.body.onClose = func() { c.failExchange(ex, errors.New("codex websocket: response body closed")) }
	c.active = ex
	c.activity.touch()
	ex.stopCtx = context.AfterFunc(ctx, func() { c.failExchange(ex, ctx.Err()) })
	c.mu.Unlock()
	if ctx.Err() != nil {
		c.failExchange(ex, ctx.Err())
		return nil, ctx.Err()
	}

	if err := c.conn.Write(ctx, websocket.MessageText, frame); err != nil {
		safeErr := safeContextError(ctx, err, errWSConnection)
		c.failExchange(ex, safeErr)
		return nil, safeErr
	}
	if ctx.Err() != nil {
		c.failExchange(ex, ctx.Err())
	}
	go c.keepalive(ex, wsPingAfter, wsPongTimeout, wsPing)
	return &http.Response{
		Status: "200 OK", StatusCode: http.StatusOK, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: c.headers.Clone(), Body: ex.body,
	}, nil
}

// Result returns a deep copy of the latest exchange summary.
func (c *WSConn) Result() WSResult {
	if c == nil {
		return WSResult{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneWSResult(c.result)
}

func cloneWSResult(r WSResult) WSResult {
	out := WSResult{ResponseID: r.ResponseID, Completed: r.Completed}
	if r.Items != nil {
		out.Items = make([]json.RawMessage, len(r.Items))
		for i := range r.Items {
			out.Items[i] = append(json.RawMessage(nil), r.Items[i]...)
		}
	}
	return out
}

// Age reports the time since the websocket handshake completed.
func (c *WSConn) Age() time.Duration {
	if c == nil {
		return 0
	}
	return time.Since(c.started)
}

// Err reports the safe terminal connection error, if any.
func (c *WSConn) Err() error {
	if c == nil {
		return errWSClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close ends the connection and any exchange in flight; it is idempotent.
func (c *WSConn) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	if c.err == nil {
		c.err = errWSClosed
	}
	ex := c.active
	c.active = nil
	c.busy = false
	c.mu.Unlock()
	if ex != nil {
		if ex.stopCtx != nil {
			ex.stopCtx()
		}
		ex.body.fail(errWSClosed)
		ex.finish()
	}
	c.cancel()
	_ = c.conn.CloseNow()
	c.transport.CloseIdleConnections()
	return nil
}

func (c *WSConn) readPump() {
	for {
		typ, data, err := c.conn.Read(c.lifeCtx)
		if err != nil {
			c.mu.Lock()
			ex := c.active
			if !c.closed {
				c.closed = true
				c.err = errWSConnection
				c.active = nil
				c.busy = false
			}
			c.mu.Unlock()
			if ex != nil {
				if ex.stopCtx != nil {
					ex.stopCtx()
				}
				ex.body.fail(errWSConnection)
				ex.finish()
			}
			c.cancel()
			c.transport.CloseIdleConnections()
			return
		}
		c.activity.touch()
		if typ != websocket.MessageText {
			continue
		}
		c.mu.Lock()
		ex := c.active
		c.mu.Unlock()
		if ex == nil {
			continue
		}
		chunk := make([]byte, 0, len(data)+8)
		chunk = append(chunk, "data: "...)
		chunk = append(chunk, data...)
		chunk = append(chunk, '\n', '\n')
		c.publishFrame(ex, data, chunk)
	}
}

func (c *WSConn) publishFrame(ex *wsExchange, data, chunk []byte) {
	if len(data) == 0 {
		return
	}
	var ev struct {
		Type     string `json:"type"`
		Response struct {
			ID string `json:"id"`
		} `json:"response"`
		Item json.RawMessage `json:"item"`
	}
	validJSON := json.Unmarshal(data, &ev) == nil
	terminal := validJSON && (ev.Type == "response.completed" || ev.Type == "response.failed" || ev.Type == "response.incomplete" || ev.Type == "error")

	b := ex.body
	b.mu.Lock()
	if b.closed || b.eof || b.queued+len(chunk) > 40<<20 {
		b.mu.Unlock()
		c.failExchange(ex, errors.New("codex websocket: response stream limit exceeded"))
		return
	}
	c.mu.Lock()
	if c.active != ex {
		c.mu.Unlock()
		b.mu.Unlock()
		return
	}
	if validJSON {
		if ev.Response.ID != "" {
			c.result.ResponseID = ev.Response.ID
		}
		if ev.Type == "response.output_item.done" && len(ev.Item) != 0 {
			c.result.Items = append(c.result.Items, append(json.RawMessage(nil), ev.Item...))
		}
	}
	if terminal {
		c.result.Completed = ev.Type == "response.completed"
		c.active = nil
		c.busy = false
		b.eof = true
	}
	c.mu.Unlock()
	b.chunks = append(b.chunks, chunk)
	b.queued += len(chunk)
	b.cond.Broadcast()
	b.mu.Unlock()
	if terminal {
		if ex.stopCtx != nil {
			ex.stopCtx()
		}
		ex.finish()
	}
}

func (c *WSConn) failExchange(ex *wsExchange, err error) {
	if err == nil {
		err = errWSConnection
	}
	c.mu.Lock()
	if c.active != ex {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.active = nil
	c.busy = false
	c.err = err
	c.mu.Unlock()
	if ex.stopCtx != nil {
		ex.stopCtx()
	}
	ex.body.fail(err)
	ex.finish()
	c.cancel()
	_ = c.conn.CloseNow()
	c.transport.CloseIdleConnections()
}

// keepalive watches one exchange: after pingAfter without any frame it sends a
// websocket ping, and after pongTimeout more without any frame or pong it
// closes the connection as lost. Every frame (data, ping, pong) counts as
// activity and restarts both timers.
func (c *WSConn) keepalive(ex *wsExchange, pingAfter, pongTimeout time.Duration, ping func(*websocket.Conn, context.Context) error) {
	pingCtx, cancel := context.WithCancel(c.lifeCtx)
	defer cancel()
	pingResult := make(chan error, 1)
	pinging := false // a ping goroutine is outstanding (its result is not read yet)
	for {
		idle := time.Since(c.activity.since())
		var wait time.Duration
		switch {
		case idle >= pingAfter+pongTimeout:
			c.failExchange(ex, errWSClientLost)
			return
		case idle >= pingAfter:
			if !pinging {
				pinging = true
				go func() { pingResult <- ping(c.conn, pingCtx) }()
			}
			wait = pingAfter + pongTimeout - idle
		default:
			wait = pingAfter - idle
		}
		timer := time.NewTimer(wait)
		select {
		case <-ex.done:
			timer.Stop()
			return
		case <-c.activity.notify:
			timer.Stop()
		case err := <-pingResult:
			timer.Stop()
			pinging = false
			if err != nil && pingCtx.Err() == nil {
				c.failExchange(ex, errWSConnection)
				return
			}
		case <-timer.C:
		}
	}
}

type wsStream struct {
	mu      sync.Mutex
	cond    *sync.Cond
	chunks  [][]byte
	offset  int
	queued  int
	eof     bool
	closed  bool
	err     error
	onClose func()
}

func newWSStream() *wsStream {
	b := &wsStream{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *wsStream) fail(err error) {
	b.mu.Lock()
	if !b.eof {
		b.err = err
		b.eof = true
	}
	b.cond.Broadcast()
	b.mu.Unlock()
}

func (b *wsStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.chunks) == 0 && !b.eof && !b.closed {
		b.cond.Wait()
	}
	if b.closed {
		return 0, errors.New("codex websocket: response body closed")
	}
	if len(b.chunks) > 0 {
		chunk := b.chunks[0]
		n := copy(p, chunk[b.offset:])
		b.offset += n
		if b.offset == len(chunk) {
			b.queued -= len(chunk)
			b.chunks[0] = nil
			b.chunks = b.chunks[1:]
			b.offset = 0
			b.cond.Broadcast()
		}
		return n, nil
	}
	if b.err != nil {
		return 0, b.err
	}
	return 0, io.EOF
}

func (b *wsStream) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	terminal := b.eof && b.err == nil
	b.chunks = nil
	b.queued = 0
	b.cond.Broadcast()
	closeFn := b.onClose
	b.mu.Unlock()
	if !terminal && closeFn != nil {
		closeFn()
	}
	return nil
}

var _ io.ReadCloser = (*wsStream)(nil)
