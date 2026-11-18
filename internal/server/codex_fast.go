package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

const fastCatalogTTL = 2 * time.Minute
const fastCatalogTimeout = 15 * time.Second
const fastCatalogMaxEntries = 128

type fastTierEntry struct {
	tier    string
	expires time.Time
}

var fastTiers = struct {
	sync.Mutex
	entries map[[32]byte]fastTierEntry
}{entries: make(map[[32]byte]fastTierEntry)}

// fastTier caches only successful, account- and route-specific resolutions.
func fastTier(ctx context.Context, cfg *config.Config, baseURL, model string, s *oauth.Session) (string, int, error) {
	ctx, cancel := context.WithTimeout(upstreamCtx(ctx, cfg), fastCatalogTimeout)
	defer cancel()
	tok, err := s.Token(ctx)
	if err != nil {
		return "", 401, err
	}
	// Hash the complete identity and route, including proxy credentials, without retaining them.
	parts, _ := json.Marshal([]string{cfg.ProviderName, baseURL, cfg.Profile, model, cfg.CodexClientVersion(), tok.AccountID, tok.AccessToken,
		cfg.ProxyChoice(cfg.Profile).Key(), os.Getenv("HTTP_PROXY"), os.Getenv("HTTPS_PROXY"), os.Getenv("NO_PROXY"),
		os.Getenv("http_proxy"), os.Getenv("https_proxy"), os.Getenv("no_proxy")})
	key := sha256.Sum256(parts)
	now := time.Now()
	fastTiers.Lock()
	entry, ok := fastTiers.entries[key]
	if ok && now.Before(entry.expires) {
		fastTiers.Unlock()
		return entry.tier, 0, nil
	}
	delete(fastTiers.entries, key)
	fastTiers.Unlock()
	models, err := codex.FetchCatalog(ctx, baseURL, tok)
	if err != nil {
		return "", 502, fmt.Errorf("codex: Fast catalog lookup failed: %w", err)
	}
	tier, err := codex.FastServiceTier(models, model)
	if err != nil {
		return "", 400, err
	}
	fastTiers.Lock()
	if len(fastTiers.entries) >= fastCatalogMaxEntries {
		var oldestKey [32]byte
		var oldest time.Time
		for k, v := range fastTiers.entries {
			if oldest.IsZero() || v.expires.Before(oldest) {
				oldestKey, oldest = k, v.expires
			}
		}
		delete(fastTiers.entries, oldestKey)
	}
	fastTiers.entries[key] = fastTierEntry{tier: tier, expires: time.Now().Add(fastCatalogTTL)}
	fastTiers.Unlock()
	return tier, 0, nil
}
