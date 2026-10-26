package oauth

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

// A request of the default client goes the way its context says: the OAuth
// calls of a login or a token refresh follow the profile's proxy.
func TestClientFollowsTheContextProxy(t *testing.T) {
	issuer := httptest.NewServer(http.NotFoundHandler())
	defer issuer.Close()
	var tunnels atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		tunnels.Add(1)
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
		_, _ = io.Copy(up, c)
		_ = up.Close()
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)

	c := &Client{Issuer: issuer.URL}
	// The answer is an error (404); only the way the request took matters.
	_, _ = c.RequestDeviceCode(proxydial.WithChoice(context.Background(), proxydial.Choice{URL: proxyURL}))
	if n := tunnels.Load(); n != 1 {
		t.Fatalf("tunnels = %d, want the call through the context's proxy", n)
	}
	_, _ = c.RequestDeviceCode(proxydial.WithChoice(context.Background(), proxydial.Choice{Direct: true}))
	if n := tunnels.Load(); n != 1 {
		t.Fatalf("tunnels = %d after a direct call, want no new tunnel", n)
	}
	proxydial.Shared.CloseIdleConnections()
}
