package server

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
)

// The levels replace each other whole: the provider's proxy beats the global
// one, the profile's own beats the provider's, and none ends the search.
func TestCodexRequestsFollowTheProviderProxy(t *testing.T) {
	var calls atomic.Int32
	backend := overloadBackend(&calls, 0) // always a complete answer
	global, viaGlobal := connectProxy(t)
	prov, viaProv := connectProxy(t)
	own, viaOwn := connectProxy(t)

	run := func(profile, providerProxy string) {
		t.Helper()
		router := connpool.NewRouter(0, 1, 1, nil)
		status, body, _, _ := codexCallWith(t, backend, &http.Client{Transport: router}, func(cfg *config.Config) {
			cfg.Profile = profile
			cfg.ConnectionsProxy = global
			cfg.Providers["codex"].Proxy = providerProxy
			cfg.Profiles = map[string]*config.Profile{
				"inherits": {Name: "inherits", Provider: "codex"},
				"direct":   {Name: "direct", Provider: "codex", Proxy: config.ProxyNone},
				"own":      {Name: "own", Provider: "codex", Proxy: own},
			}
		})
		if status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
			t.Fatalf("%s: status %d, body %q", profile, status, body)
		}
	}
	tunnels := func() [3]int32 { return [3]int32{viaGlobal.Load(), viaProv.Load(), viaOwn.Load()} }

	run("inherits", prov)
	if got := tunnels(); got != [3]int32{0, 1, 0} {
		t.Fatalf("inherits, provider proxy: tunnels global/provider/own = %v, want 0/1/0", got)
	}
	run("own", prov)
	if got := tunnels(); got != [3]int32{0, 1, 1} {
		t.Fatalf("own beats the provider's: tunnels = %v, want 0/1/1", got)
	}
	run("direct", prov)
	if got := tunnels(); got != [3]int32{0, 1, 1} {
		t.Fatalf("none of the profile: tunnels = %v, want no new tunnel", got)
	}
	run("inherits", config.ProxyNone)
	if got := tunnels(); got != [3]int32{0, 1, 1} {
		t.Fatalf("none of the provider beats the global proxy: tunnels = %v, want no new tunnel", got)
	}
	run("inherits", "")
	if got := tunnels(); got != [3]int32{1, 1, 1} {
		t.Fatalf("no provider proxy: tunnels = %v, want the global one used (1/1/1)", got)
	}
}
