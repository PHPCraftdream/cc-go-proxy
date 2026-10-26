package server

import (
	"fmt"
	"net/http"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
)

// applyConnectionLimits sizes the connection pools of every upstream client
// from connections.active / .total; it returns the values applied.
func applyConnectionLimits(cfg *config.Config) (active, total int) {
	active, total = cfg.ConnectionLimits()
	for _, rt := range []http.RoundTripper{codex.HTTPClient.Transport, upstreamClient.Transport, openAITransport} {
		if t, ok := rt.(*connpool.Transport); ok {
			t.SetLimits(active, total)
		}
	}
	return active, total
}

// reapplyConnectionLimits applies next's limits, printing them when they changed.
func reapplyConnectionLimits(prev, next *config.Config) {
	pa, pt := prev.ConnectionLimits()
	a, t := applyConnectionLimits(next)
	if a != pa || t != pt {
		fmt.Printf("   upstream connections: %d active, %d total\n", a, t)
	}
}
