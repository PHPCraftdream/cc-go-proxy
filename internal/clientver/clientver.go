// Package clientver keeps the Codex client version of config.ktav
// (`codex.client_version`) current: it raises it when the backend asks for a
// newer client or a newer Codex CLI is out, and refreshes the managed model
// mappings that a newer version may unlock.

package clientver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/cfgedit"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

// LatestReleaseURL is the newest Codex CLI release, as the official client
// itself looks it up.
const LatestReleaseURL = "https://api.github.com/repos/openai/codex/releases/latest"

// lookupCooldown spaces out release lookups: a backend that keeps refusing
// must not turn every request into a GitHub call.
const lookupCooldown = 10 * time.Minute

var tagVersion = regexp.MustCompile(`(\d+\.\d+\.\d+)$`)

// Latest returns the version of the newest Codex CLI release.
func Latest(ctx context.Context) (string, error) {
	return latestFrom(ctx, proxydial.HTTPClient(20*time.Second), LatestReleaseURL)
}

func latestFrom(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ccgp")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("latest Codex CLI release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("latest Codex CLI release: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("latest Codex CLI release: HTTP %d", resp.StatusCode)
	}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", fmt.Errorf("latest Codex CLI release: %w", err)
	}
	m := tagVersion.FindStringSubmatch(rel.Tag) // "rust-v0.169.0"
	if m == nil {
		return "", fmt.Errorf("latest Codex CLI release: unexpected tag %q", rel.Tag)
	}
	return m[1], nil
}

// ErrCooldown is returned when a release lookup ran too recently.
var ErrCooldown = errors.New("the latest Codex CLI release was looked up moments ago")

// Upgrader raises codex.client_version, one raise at a time per process.
type Upgrader struct {
	// Latest looks up the newest Codex CLI version (Latest when nil).
	Latest func(context.Context) (string, error)

	mu     sync.Mutex
	looked time.Time
}

// Default is the process-wide upgrader.
var Default = &Upgrader{}

// Target is the version to raise current to: required when the backend named
// one, else the newest Codex CLI release. "" when nothing is newer than current.
func (u *Upgrader) Target(ctx context.Context, current, required string) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.target(ctx, current, required)
}

func (u *Upgrader) target(ctx context.Context, current, required string) (string, error) {
	target := required
	if !config.ValidClientVersion(target) {
		if !u.looked.IsZero() && time.Since(u.looked) < lookupCooldown {
			return "", ErrCooldown
		}
		u.looked = time.Now()
		latest := u.Latest
		if latest == nil {
			latest = Latest
		}
		var err error
		if target, err = latest(ctx); err != nil {
			return "", err
		}
	}
	if config.CompareClientVersions(target, current) <= 0 {
		return "", nil
	}
	return target, nil
}

// Raise writes a newer codex.client_version into the config file at path:
// required when the backend named one, else the newest Codex CLI release. It
// never lowers it. to is "" when nothing newer was found.
func (u *Upgrader) Raise(ctx context.Context, path, current, required string) (to string, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	to, err = u.target(ctx, current, required)
	if err != nil || to == "" {
		return "", err
	}
	if _, _, err := cfgedit.RaiseCodexClientVersion(path, to); err != nil {
		return "", err
	}
	return to, nil
}

// ManagedFrom converts a catalog selection into the config's managed block.
func ManagedFrom(sel codex.Selection) cfgedit.ManagedModels {
	return cfgedit.ManagedModels{Rules: []config.ModelRule{
		{Match: "*fable*", Model: sel.Fable, ContextWindow: sel.FableWindow},
		{Match: "*opus*", Model: sel.Opus, ContextWindow: sel.OpusWindow},
		{Match: "*sonnet*", Model: sel.Sonnet, ContextWindow: sel.SonnetWindow},
		{Match: "*haiku*", Model: sel.Haiku, ContextWindow: sel.HaikuWindow},
		// Anything else (custom names) goes to the Sonnet model.
		{Match: "*", Model: sel.Sonnet, ContextWindow: sel.SonnetWindow},
	}}
}

// RefreshMappings rewrites, from the account catalog, the managed model block
// of every profile of provider that has one (hand-written mappings are left
// alone). ctx carries the client version and the way out. It returns the
// selection and the profiles it changed.
func RefreshMappings(ctx context.Context, cfg *config.Config, provider string, tok *oauth.Tokens) (codex.Selection, []string, error) {
	pr := cfg.Providers[provider]
	if pr == nil || pr.Type != config.ProviderTypeCodex {
		return codex.Selection{}, nil, fmt.Errorf("%q is not a codex provider", provider)
	}
	list, err := codex.FetchCatalog(ctx, pr.BaseURL, tok)
	if err != nil {
		return codex.Selection{}, nil, err
	}
	sel, err := codex.Pick(list)
	if err != nil {
		return sel, nil, err
	}
	data, err := os.ReadFile(cfg.ConfigFile)
	if err != nil {
		return sel, nil, err
	}
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	var changed []string
	for _, name := range names {
		if cfg.Profiles[name].Provider != provider {
			continue
		}
		out, err := cfgedit.ReplaceManaged(string(data), name, ManagedFrom(sel))
		if err != nil || out == string(data) {
			continue // no managed block, or already current
		}
		if err := cfgedit.ApplyManaged(cfg.ConfigFile, name, ManagedFrom(sel)); err != nil {
			return sel, changed, fmt.Errorf("update mapping of profile %q: %w", name, err)
		}
		changed = append(changed, name)
		if data, err = os.ReadFile(cfg.ConfigFile); err != nil {
			return sel, changed, err
		}
	}
	return sel, changed, nil
}
