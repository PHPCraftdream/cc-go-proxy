package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
)

func TestApplyConnectionLimits(t *testing.T) {
	t.Cleanup(func() { applyConnectionLimits(&config.Config{}) })
	if a, n := applyConnectionLimits(&config.Config{ConnectionsActive: 3, ConnectionsTotal: 4}); a != 3 || n != 4 {
		t.Fatalf("applied %d/%d", a, n)
	}
	for name, rt := range map[string]http.RoundTripper{
		"codex": codex.HTTPClient.Transport, "anthropic": upstreamClient.Transport, "openai": openAITransport,
	} {
		tr, ok := rt.(*connpool.Router)
		if !ok {
			t.Errorf("%s: transport %T, want *connpool.Router", name, rt)
			continue
		}
		if a, n := tr.Limits(); a != 3 || n != 4 {
			t.Errorf("%s: limits %d/%d, want 3/4", name, a, n)
		}
	}
}

func TestWriteUpstreamStats(t *testing.T) {
	ts := upstreamTransports()
	if len(ts) != 3 {
		t.Fatalf("transports = %d, want codex, anthropic and openai", len(ts))
	}
	for _, r := range ts {
		r.Stats() // other tests in the package count too: start from zero
	}
	var b strings.Builder
	writeUpstreamStats(&b, time.Unix(0, 0), ts)
	if b.Len() != 0 {
		t.Errorf("quiet transports logged %q", b.String())
	}

	// A client that retried a dropped connection gets a line.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = c.Close()
		}
	}))
	defer srv.Close()
	router := connpool.NewRouter(0, 1, 1, nil)
	resp, err := (&http.Client{Transport: router}).Post(srv.URL, "text/plain", strings.NewReader("x"))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a dropped connection produced an answer")
	}
	b.Reset()
	writeUpstreamStats(&b, time.Unix(0, 0), map[string]*connpool.Router{"codex": router})
	if got := b.String(); !strings.Contains(got, "upstream stats [codex]: ") || !strings.Contains(got, "0 after connect failures, 2 after broken connections") {
		t.Errorf("stats line = %q", got)
	}
}
