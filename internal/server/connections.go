package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

// applyConnectionLimits sizes the connection pools of every upstream client
// from connections.active / .total; it returns the values applied.
func applyConnectionLimits(cfg *config.Config) (active, total int) {
	active, total = cfg.ConnectionLimits()
	for _, rt := range []http.RoundTripper{codex.HTTPClient.Transport, upstreamClient.Transport, openAITransport} {
		if r, ok := rt.(*connpool.Router); ok {
			r.SetLimits(active, total)
		}
	}
	return active, total
}

// reapplyConnectionLimits applies next's limits, printing them and the proxy
// when they changed (config.Load has already made next's proxy the default).
func reapplyConnectionLimits(prev, next *config.Config) {
	pa, pt := prev.ConnectionLimits()
	a, t := applyConnectionLimits(next)
	if a != pa || t != pt {
		fmt.Printf("   upstream connections: %d active, %d total\n", a, t)
	}
	if prev.ConnectionsProxy != next.ConnectionsProxy {
		fmt.Printf("   upstream proxy: %s\n", proxyText(next))
	}
}

// proxyText names the proxy for the log, without its password.
func proxyText(cfg *config.Config) string {
	if cfg.ConnectionsProxy == "" {
		return "none"
	}
	return proxydial.Redact(cfg.ConnectionsProxy)
}

// statsEvery is how often the connection counts of the upstream clients are logged.
const statsEvery = 10 * time.Minute

// upstreamTransports names the connection pools of the upstream clients.
func upstreamTransports() map[string]*connpool.Router {
	out := map[string]*connpool.Router{}
	for name, rt := range map[string]http.RoundTripper{
		"codex": codex.HTTPClient.Transport, "anthropic": upstreamClient.Transport, "openai": openAITransport,
	} {
		if r, ok := rt.(*connpool.Router); ok {
			out[name] = r
		}
	}
	return out
}

// writeUpstreamStats prints one line per client that did anything since the
// last call: dials and their protocol, warm-ups, retries.
func writeUpstreamStats(w io.Writer, now time.Time, ts map[string]*connpool.Router) {
	names := make([]string, 0, len(ts))
	for n := range ts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if s := ts[n].Stats().String(); s != "" {
			_, _ = fmt.Fprintf(w, "[%s] upstream stats [%s]: %s\n", now.Format("15:04:05"), n, s)
		}
	}
}

// logUpstreamStats writes the stats lines every statsEvery, for the server's life.
func logUpstreamStats(w io.Writer) {
	ts := upstreamTransports()
	for now := range time.Tick(statsEvery) {
		writeUpstreamStats(w, now, ts)
	}
}

// upstreamCtx puts the profile's way out (its own proxy, none, or the global
// one; see config.ProxyChoice) on the context of an upstream request. The
// clients' connpool.Router sends the request over the pools of that choice.
func upstreamCtx(ctx context.Context, cfg *config.Config) context.Context {
	return proxydial.WithChoice(ctx, cfg.ProxyChoice(cfg.Profile))
}
