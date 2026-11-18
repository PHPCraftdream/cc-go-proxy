package tier

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/catalog"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/rt"
)

const fastCatalogTTL = 2 * time.Minute

const fastCatalogTimeout = 15 * time.Second

const FastCatalogMaxEntries = 128

type FastTierEntry struct {
	Tier    string
	Expires time.Time
}

var FastTiers = struct {
	sync.Mutex
	Entries map[[32]byte]FastTierEntry
}{Entries: make(map[[32]byte]FastTierEntry)}

// fastTier caches only successful, account- and route-specific resolutions.
func FastTier(ctx context.Context, cfg *config.Config, baseURL, model string, s *oauth.Session) (string, int, error) {
	ctx, cancel := context.WithTimeout(rt.UpstreamCtx(ctx, cfg), fastCatalogTimeout)
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
	FastTiers.Lock()
	entry, ok := FastTiers.Entries[key]
	if ok && now.Before(entry.Expires) {
		FastTiers.Unlock()
		return entry.Tier, 0, nil
	}
	delete(FastTiers.Entries, key)
	FastTiers.Unlock()
	models, err := catalog.FetchCatalog(ctx, baseURL, tok)
	if err != nil {
		return "", 502, fmt.Errorf("codex: Fast catalog lookup failed: %w", err)
	}
	tier, err := catalog.FastServiceTier(models, model)
	if err != nil {
		return "", 400, err
	}
	FastTiers.Lock()
	if len(FastTiers.Entries) >= FastCatalogMaxEntries {
		var oldestKey [32]byte
		var oldest time.Time
		for k, v := range FastTiers.Entries {
			if oldest.IsZero() || v.Expires.Before(oldest) {
				oldestKey, oldest = k, v.Expires
			}
		}
		delete(FastTiers.Entries, oldestKey)
	}
	FastTiers.Entries[key] = FastTierEntry{Tier: tier, Expires: time.Now().Add(fastCatalogTTL)}
	FastTiers.Unlock()
	return tier, 0, nil
}
