package proxydial

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func target(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello through "+r.Host)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func pipe(a, b net.Conn) {
	go func() { _, _ = io.Copy(a, b); _ = a.Close() }()
	_, _ = io.Copy(b, a)
	_ = b.Close()
}

// socksServer is a minimal SOCKS5 proxy: optional password auth, CONNECT,
// reply code refuse (0: success). hosts receives the requested host names.
func socksServer(t *testing.T, user, pass string, refuse byte) (string, chan string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	hosts := make(chan string, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				b := make([]byte, 2)
				if _, err := io.ReadFull(c, b); err != nil {
					return
				}
				methods := make([]byte, b[1])
				_, _ = io.ReadFull(c, methods)
				if user != "" {
					_, _ = c.Write([]byte{5, 2})
					h := make([]byte, 2)
					_, _ = io.ReadFull(c, h)
					u := make([]byte, h[1])
					_, _ = io.ReadFull(c, u)
					_, _ = io.ReadFull(c, h[:1])
					p := make([]byte, h[0])
					_, _ = io.ReadFull(c, p)
					if string(u) != user || string(p) != pass {
						_, _ = c.Write([]byte{1, 1})
						return
					}
					_, _ = c.Write([]byte{1, 0})
				} else {
					_, _ = c.Write([]byte{5, 0})
				}
				head := make([]byte, 4)
				if _, err := io.ReadFull(c, head); err != nil {
					return
				}
				var host string
				switch head[3] {
				case 1:
					ip := make([]byte, 4)
					_, _ = io.ReadFull(c, ip)
					host = net.IP(ip).String()
				case 3:
					n := make([]byte, 1)
					_, _ = io.ReadFull(c, n)
					name := make([]byte, n[0])
					_, _ = io.ReadFull(c, name)
					host = string(name)
				}
				pb := make([]byte, 2)
				_, _ = io.ReadFull(c, pb)
				hosts <- host
				if refuse != 0 {
					_, _ = c.Write([]byte{5, refuse, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				up, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb)))))
				if err != nil {
					_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				pipe(c, up)
			}(c)
		}
	}()
	return ln.Addr().String(), hosts
}

// connectProxy is a minimal HTTP CONNECT proxy; with auth set it wants that
// Proxy-Authorization header.
func connectProxy(t *testing.T, auth string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		if auth != "" && r.Header.Get("Proxy-Authorization") != auth {
			http.Error(w, "auth", http.StatusProxyAuthRequired)
			return
		}
		up, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		pipe(c, up)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func getVia(t *testing.T, proxy, url string) (string, error) {
	t.Helper()
	u, err := Parse(proxy)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: Dialer(u, nil)}, Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func TestSocks5(t *testing.T) {
	srv := target(t)
	addr, hosts := socksServer(t, "", "", 0)
	got, err := getVia(t, "socks5://"+addr, srv.URL)
	if err != nil || !strings.HasPrefix(got, "hello") {
		t.Fatalf("got %q, %v", got, err)
	}
	if h := <-hosts; h != "127.0.0.1" {
		t.Errorf("proxy asked for %q", h)
	}
}

func TestSocks5SendsHostNames(t *testing.T) {
	srv := target(t)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	addr, hosts := socksServer(t, "", "", 0)
	if _, err := getVia(t, "socks5h://"+addr, "http://localhost:"+port); err != nil {
		t.Fatal(err)
	}
	if h := <-hosts; h != "localhost" {
		t.Errorf("proxy asked for %q, want the name", h)
	}
}

func TestSocks5Password(t *testing.T) {
	srv := target(t)
	addr, _ := socksServer(t, "me", "s3cret", 0)
	if got, err := getVia(t, "socks5://me:s3cret@"+addr, srv.URL); err != nil || !strings.HasPrefix(got, "hello") {
		t.Fatalf("right password: %q, %v", got, err)
	}
	if _, err := getVia(t, "socks5://me:wrong@"+addr, srv.URL); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("wrong password: %v", err)
	}
	if _, err := getVia(t, "socks5://"+addr, srv.URL); err == nil || !strings.Contains(err.Error(), "wants a user name") {
		t.Errorf("no password: %v", err)
	}
}

func TestSocks5Refused(t *testing.T) {
	srv := target(t)
	addr, _ := socksServer(t, "", "", 5)
	if _, err := getVia(t, "socks5://"+addr, srv.URL); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err = %v", err)
	}
}

func TestHTTPConnect(t *testing.T) {
	srv := target(t)
	p := connectProxy(t, "")
	if got, err := getVia(t, p.URL, srv.URL); err != nil || !strings.HasPrefix(got, "hello") {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestHTTPConnectAuth(t *testing.T) {
	srv := target(t)
	p := connectProxy(t, "Basic bWU6czNjcmV0") // me:s3cret
	host := strings.TrimPrefix(p.URL, "http://")
	if got, err := getVia(t, "http://me:s3cret@"+host, srv.URL); err != nil || !strings.HasPrefix(got, "hello") {
		t.Fatalf("with auth: %q, %v", got, err)
	}
	if _, err := getVia(t, p.URL, srv.URL); err == nil || !strings.Contains(err.Error(), "407") {
		t.Errorf("without auth: %v", err)
	}
}

func TestStuckProxyObeysTheContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }() // accepts, never answers
		}
	}()
	for _, scheme := range []string{"socks5", "http"} {
		u, _ := Parse(scheme + "://" + ln.Addr().String())
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		start := time.Now()
		_, err := Dialer(u, nil)(ctx, "tcp", "example.com:443")
		cancel()
		if err == nil || time.Since(start) > 5*time.Second {
			t.Errorf("%s: err %v after %s", scheme, err, time.Since(start))
		}
	}
}

func TestParse(t *testing.T) {
	for _, ok := range []string{"socks5://127.0.0.1:1080", "socks5h://u:p@proxy:1080", "http://proxy:3128", "https://proxy:443/"} {
		if _, err := Parse(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"ftp://proxy:21", "socks5://proxy", "proxy:1080", "http://proxy:3128/path", "socks5://:1080"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestRedact(t *testing.T) {
	if got := Redact("socks5://me:s3cret@proxy:1080"); got != "socks5://me:***@proxy:1080" {
		t.Errorf("Redact = %q", got)
	}
	if got := Redact("http://proxy:3128"); got != "http://proxy:3128" {
		t.Errorf("Redact = %q", got)
	}
	if _, err := Parse("ftp://me:s3cret@proxy:21"); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error leaks the password: %v", err)
	}
}

func TestHTTPClientUsesTheDefault(t *testing.T) {
	srv := target(t)
	addr, hosts := socksServer(t, "", "", 0)
	t.Cleanup(func() { SetDefault(nil) })
	u, _ := Parse("socks5://" + addr)
	SetDefault(u)
	u.Host = "changed:1" // SetDefault keeps its own copy
	resp, err := HTTPClient(5 * time.Second).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	select {
	case <-hosts:
	default:
		t.Fatal("the request did not go through the default proxy")
	}
	SetDefault(nil)
	if Default() != nil {
		t.Error("SetDefault(nil) kept a proxy")
	}
	resp, err = HTTPClient(5 * time.Second).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	select {
	case h := <-hosts:
		t.Fatalf("direct request went through the proxy (%s)", h)
	default:
	}
}

func TestChoiceKeysAndText(t *testing.T) {
	u, _ := Parse("socks5://me:s3cret@127.0.0.1:1080")
	cases := []struct {
		c         Choice
		key, text string
	}{
		{Choice{}, "env", "environment"},
		{Choice{Direct: true}, "direct", "none"},
		{Choice{URL: u}, "proxy:socks5://me:s3cret@127.0.0.1:1080", "socks5://me:***@127.0.0.1:1080"},
	}
	for _, tc := range cases {
		if tc.c.Key() != tc.key || tc.c.String() != tc.text {
			t.Errorf("%+v: key %q, text %q; want %q, %q", tc.c, tc.c.Key(), tc.c.String(), tc.key, tc.text)
		}
	}
}

func TestChoiceForContext(t *testing.T) {
	t.Cleanup(func() { SetDefault(nil) })
	d, _ := Parse("socks5://127.0.0.1:1")
	own, _ := Parse("http://127.0.0.1:2")
	bg := context.Background()
	if c := ChoiceFor(bg); c.URL != nil || c.Direct {
		t.Errorf("no default: %+v", c)
	}
	SetDefault(d)
	if c := ChoiceFor(bg); c.URL == nil || c.URL.Host != "127.0.0.1:1" {
		t.Errorf("default: %+v", c)
	}
	if c := ChoiceFor(WithChoice(bg, Choice{URL: own})); c.URL.Host != "127.0.0.1:2" {
		t.Errorf("own proxy: %+v", c)
	}
	if c := ChoiceFor(WithChoice(bg, Choice{Direct: true})); !c.Direct || c.URL != nil {
		t.Errorf("direct: %+v", c)
	}
	// An explicit environment choice beats the default.
	if c := ChoiceFor(WithChoice(bg, Choice{})); c.URL != nil || c.Direct {
		t.Errorf("explicit env choice fell back to the default: %+v", c)
	}
}

func TestApplyChoice(t *testing.T) {
	proxyURL, _ := Parse("http://proxy:3128")
	envProxy := func(*http.Request) (*url.URL, error) { return proxyURL, nil }
	tr := &http.Transport{Proxy: envProxy}
	Apply(tr, Choice{})
	if tr.Proxy == nil || tr.DialContext != nil {
		t.Error("the zero choice changed the transport")
	}
	Apply(tr, Choice{Direct: true})
	if tr.Proxy != nil || tr.DialContext != nil {
		t.Error("Direct kept the environment proxy or set a dialer")
	}
	tr = &http.Transport{Proxy: envProxy}
	Apply(tr, Choice{URL: proxyURL})
	if tr.Proxy != nil || tr.DialContext == nil {
		t.Error("a proxy URL kept the environment proxy or set no dialer")
	}
}

// The shared transport sends each request the way its context says.
func TestTransportPicksPerRequest(t *testing.T) {
	srv := target(t)
	socksAddr, hosts := socksServer(t, "", "", 0)
	other, otherHosts := socksServer(t, "", "", 0)
	socks, _ := Parse("socks5://" + socksAddr)
	socks2, _ := Parse("socks5://" + other)
	client := HTTPClient(5 * time.Second)
	do := func(c Choice) {
		t.Helper()
		req, _ := http.NewRequestWithContext(WithChoice(context.Background(), c), http.MethodGet, srv.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	got := func(ch chan string) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	do(Choice{URL: socks})
	if !got(hosts) || got(otherHosts) {
		t.Fatal("request did not go through the first proxy only")
	}
	do(Choice{URL: socks2})
	if got(hosts) || !got(otherHosts) {
		t.Fatal("request did not go through the second proxy only")
	}
	do(Choice{Direct: true})
	if got(hosts) || got(otherHosts) {
		t.Fatal("a direct request used a proxy")
	}
	Shared.CloseIdleConnections()
}
