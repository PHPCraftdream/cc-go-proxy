package server

import (
	"net/http"
	"testing"

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
		tr, ok := rt.(*connpool.Transport)
		if !ok {
			t.Errorf("%s: transport %T, want *connpool.Transport", name, rt)
			continue
		}
		if a, n := tr.Limits(); a != 3 || n != 4 {
			t.Errorf("%s: limits %d/%d, want 3/4", name, a, n)
		}
	}
}
