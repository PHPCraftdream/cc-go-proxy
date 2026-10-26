// Package proxydial opens TCP connections through a SOCKS5 or HTTP(S) proxy,
// so callers that dial themselves (connpool's warm-ups) reach the backend the
// same way as net/http's own proxy support.
//
//	socks5://[user:pass@]host:port   SOCKS5 (RFC 1928, password auth RFC 1929)
//	socks5h://…                      the same; the target's name is always resolved by the proxy
//	http://[user:pass@]host:port     HTTP CONNECT, Basic auth
//	https://…                        HTTP CONNECT over TLS to the proxy
package proxydial

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DialFunc is the shape of net.Dialer.DialContext.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Parse checks a proxy URL: a supported scheme and a host with a port.
func Parse(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("proxy %q: %w", Redact(raw), err)
	}
	switch u.Scheme {
	case "socks5", "socks5h", "http", "https":
	default:
		return nil, fmt.Errorf("proxy %q: scheme must be socks5, socks5h, http or https", Redact(raw))
	}
	if u.Hostname() == "" || u.Port() == "" {
		return nil, fmt.Errorf("proxy %q: want scheme://host:port", Redact(raw))
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("proxy %q: no path expected", Redact(raw))
	}
	return u, nil
}

// Redact hides the password of a proxy URL for logs and output.
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, ok := u.User.Password(); ok {
		u.User = url.UserPassword(u.User.Username(), "***")
	}
	return strings.Replace(u.String(), "%2A%2A%2A", "***", 1)
}

// Dialer returns a DialFunc that reaches addr through the proxy u; forward
// opens the connection to the proxy itself (nil: a net.Dialer).
func Dialer(u *url.URL, forward DialFunc) DialFunc {
	if forward == nil {
		forward = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	proxyAddr := u.Host
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := forward(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("proxy %s: %w", proxyAddr, err)
		}
		// The handshake obeys the context: a stuck proxy must not hang the dial.
		if dl, ok := ctx.Deadline(); ok {
			_ = c.SetDeadline(dl)
		}
		stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Unix(1, 0)) })
		var out net.Conn
		switch u.Scheme {
		case "socks5", "socks5h":
			out, err = socks5(c, u, addr)
		default:
			out, err = connect(c, u, addr)
		}
		if !stop() && err == nil {
			err = ctx.Err()
		}
		if err != nil {
			_ = c.Close()
			if ctx.Err() != nil {
				err = fmt.Errorf("%w (%w)", ctx.Err(), err)
			}
			return nil, fmt.Errorf("proxy %s: %w", proxyAddr, err)
		}
		_ = c.SetDeadline(time.Time{})
		return out, nil
	}
}

// socks5 runs the SOCKS5 greeting, optional password auth and CONNECT to addr,
// sending the host name so the proxy resolves it.
func socks5(c net.Conn, u *url.URL, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("bad port in %q", addr)
	}
	methods := []byte{0x00} // no auth
	if u.User != nil {
		methods = []byte{0x02, 0x00} // password, then none
	}
	if _, err := c.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return nil, err
	}
	var reply [2]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		return nil, err
	}
	if reply[0] != 0x05 {
		return nil, errors.New("not a SOCKS5 proxy")
	}
	switch reply[1] {
	case 0x00:
	case 0x02:
		if u.User == nil {
			return nil, errors.New("socks5: the proxy wants a user name and password")
		}
		user := u.User.Username()
		pass, _ := u.User.Password()
		if len(user) > 255 || len(pass) > 255 {
			return nil, errors.New("socks5: user name or password longer than 255 bytes")
		}
		msg := append([]byte{0x01, byte(len(user))}, user...)
		msg = append(append(msg, byte(len(pass))), pass...)
		if _, err := c.Write(msg); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(c, reply[:]); err != nil {
			return nil, err
		}
		if reply[1] != 0x00 {
			return nil, errors.New("socks5: user name or password rejected")
		}
	default:
		return nil, errors.New("socks5: the proxy accepts none of our authentication methods")
	}

	req := []byte{0x05, 0x01, 0x00} // CONNECT
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(append(req, 0x01), ip4...)
		} else {
			req = append(append(req, 0x04), ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return nil, errors.New("socks5: host name longer than 255 bytes")
		}
		req = append(append(req, 0x03, byte(len(host))), host...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := c.Write(req); err != nil {
		return nil, err
	}
	var head [4]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return nil, err
	}
	if head[0] != 0x05 {
		return nil, errors.New("socks5: bad reply")
	}
	if head[1] != 0x00 {
		return nil, fmt.Errorf("socks5: connect to %s refused: %s", addr, socksReply(head[1]))
	}
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		var n [1]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return nil, err
		}
		skip = int(n[0])
	default:
		return nil, errors.New("socks5: bad address type in reply")
	}
	if _, err := io.CopyN(io.Discard, c, int64(skip+2)); err != nil { // bound address and port
		return nil, err
	}
	return c, nil
}

func socksReply(code byte) string {
	switch code {
	case 0x01:
		return "general failure"
	case 0x02:
		return "not allowed by the proxy's rules"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "TTL expired"
	case 0x07:
		return "command not supported"
	case 0x08:
		return "address type not supported"
	}
	return fmt.Sprintf("code %d", code)
}

// connect opens an HTTP CONNECT tunnel to addr; for an https proxy the
// connection to the proxy is TLS first.
func connect(c net.Conn, u *url.URL, addr string) (net.Conn, error) {
	if u.Scheme == "https" {
		tc := tls.Client(c, &tls.Config{ServerName: u.Hostname()})
		if err := tc.Handshake(); err != nil {
			return nil, err
		}
		c = tc
	}
	req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n"
	if u.User != nil {
		pass, _ := u.User.Password()
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pass)) + "\r\n"
	}
	if _, err := io.WriteString(c, req+"\r\n"); err != nil {
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("http proxy: CONNECT %s: %s", addr, resp.Status)
	}
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: c, r: br}, nil
	}
	return c, nil
}

// bufferedConn returns bytes the CONNECT reply reader read ahead before the conn's own.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

var defaultProxy atomic.Pointer[url.URL]

// SetDefault makes u (nil: none) the proxy of requests that carry no Choice of
// their own: the command line's logins, --doctor, --update. config.Load sets it
// from connections.proxy.
func SetDefault(u *url.URL) {
	if u != nil {
		c := *u
		u = &c
	}
	defaultProxy.Store(u)
}

// Default returns the default proxy, nil when none is set.
func Default() *url.URL { return defaultProxy.Load() }

// Choice is how one request reaches the internet: through URL, Direct (no
// proxy at all, the environment's included), or, when both are zero, the way
// net/http picks it (HTTPS_PROXY / HTTP_PROXY / NO_PROXY).
type Choice struct {
	URL    *url.URL
	Direct bool
}

// Key names the choice: one connection pool per key.
func (c Choice) Key() string {
	switch {
	case c.URL != nil:
		return "proxy:" + c.URL.String()
	case c.Direct:
		return "direct"
	}
	return "env"
}

// String is the log form, without a password.
func (c Choice) String() string {
	switch {
	case c.URL != nil:
		return Redact(c.URL.String())
	case c.Direct:
		return "none"
	}
	return "environment"
}

type choiceKey struct{}

// WithChoice returns ctx carrying c: requests made with it use that way out.
func WithChoice(ctx context.Context, c Choice) context.Context {
	return context.WithValue(ctx, choiceKey{}, c)
}

// ChoiceFor returns the choice a request with ctx uses: its own, else the
// default proxy, else the environment's.
func ChoiceFor(ctx context.Context) Choice {
	if c, ok := ctx.Value(choiceKey{}).(Choice); ok {
		return c
	}
	return Choice{URL: Default()}
}

// Apply makes tr reach the internet the way c says: through the proxy (tr's
// own dialer then reaches it and the environment is not consulted), directly,
// or, for the zero Choice, as tr already does.
func Apply(tr *http.Transport, c Choice) {
	switch {
	case c.URL != nil:
		tr.Proxy = nil
		tr.DialContext = Dialer(c.URL, tr.DialContext)
	case c.Direct:
		tr.Proxy = nil
	}
}

// Transport is an http.RoundTripper that picks the way out per request (see
// ChoiceFor) and keeps one connection pool per choice.
type Transport struct {
	mu sync.Mutex
	m  map[string]*http.Transport
}

// Shared serves the clients of HTTPClient.
var Shared = &Transport{}

func (t *Transport) get(c Choice) *http.Transport {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]*http.Transport{}
	}
	k := c.Key()
	tr := t.m[k]
	if tr == nil {
		tr = http.DefaultTransport.(*http.Transport).Clone()
		Apply(tr, c)
		t.m[k] = tr
	}
	return tr
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.get(ChoiceFor(req.Context())).RoundTrip(req)
}

// CloseIdleConnections closes the idle connections of every pool.
func (t *Transport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, tr := range t.m {
		tr.CloseIdleConnections()
	}
}

// HTTPClient returns a client with the given timeout whose requests go the way
// their context says (see ChoiceFor).
func HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: Shared}
}
