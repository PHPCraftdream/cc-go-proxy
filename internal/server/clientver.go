package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/clientver"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

// clientVersionTimeout bounds the release lookup, the config edit and the
// catalog read done while the client waits.
const clientVersionTimeout = 30 * time.Second

// upgrader raises codex.client_version; tests replace it.
var upgrader = clientver.Default

// raiseClientVersion answers a backend refusal that asks for a newer client:
// it raises codex.client_version in config.ktav to the version the backend
// named (else the newest Codex CLI release) and refreshes the managed model
// mappings of the provider's profiles. The server reloads the config on the
// next request. note goes to the client and the log; raised tells whether a
// retry can succeed now.
func raiseClientVersion(cfg *config.Config, s *oauth.Session, required string) (note string, raised bool) {
	current := cfg.CodexClientVersion()
	if cfg.IsEnvMode() {
		return fmt.Sprintf("the backend wants a Codex client newer than %s; env-only mode has no config.ktav to raise codex.client_version in", current), false
	}
	ctx, cancel := context.WithTimeout(upstreamCtx(context.Background(), cfg), clientVersionTimeout)
	defer cancel()
	to, err := upgrader.Raise(ctx, cfg.ConfigFile, current, required)
	if err != nil {
		return fmt.Sprintf("the backend wants a Codex client newer than %s, and ccgp could not raise codex.client_version (%v): set it in %s and run `ccgp --models apply`", current, err, cfg.ConfigFile), false
	}
	if to == "" {
		return fmt.Sprintf("the backend wants a Codex client newer than %s, the newest Codex CLI release", current), false
	}
	note = fmt.Sprintf("ccgp raised codex.client_version %s -> %s", current, to)
	tok, err := s.Token(ctx)
	var changed []string
	if err == nil {
		_, changed, err = clientver.RefreshMappings(codex.WithClientVersion(ctx, to), cfg, cfg.ProviderName, tok)
	}
	switch {
	case err != nil:
		note += fmt.Sprintf("; model mappings not refreshed (%v): run `ccgp --models apply`", err)
	case len(changed) > 0:
		note += "; refreshed the model mapping of " + strings.Join(changed, ", ")
	}
	_, _ = fmt.Fprintf(logOut, "[%s] %s\n", time.Now().Format("15:04:05"), note)
	return note, true
}
