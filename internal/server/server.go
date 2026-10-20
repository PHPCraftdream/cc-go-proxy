// Package server implements the HTTP proxy server that translates between
// Claude API format and upstream providers (OpenAI-compatible, Codex OAuth).
//
// Requests arrive on /call/{token}/v1/messages; the token selects a profile,
// which selects the provider and model mapping.
package server

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/converter"
	"github.com/claude-code-proxy/proxy/internal/creds"
	"github.com/claude-code-proxy/proxy/internal/oauth"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

const (
	// ProxyVersion is the current version of the proxy
	ProxyVersion = "1.0.0"
)

// sessions caches one OAuth session per codex provider.
type sessions struct {
	store creds.Store
	mu    sync.Mutex
	m     map[string]*oauth.Session
}

func (s *sessions) get(provider string) *oauth.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*oauth.Session{}
	}
	if ss, ok := s.m[provider]; ok {
		return ss
	}
	ss := &oauth.Session{Store: oauth.Store{Path: s.store.OAuthPath(provider)}, Client: &oauth.Client{}}
	s.m[provider] = ss
	return ss
}

// Start initializes and starts the HTTP server (blocks).
func Start(cfg *config.Config) error {
	dir, err := creds.DefaultDir()
	if err != nil {
		return err
	}
	store := creds.Store{Dir: dir}
	index, build := profileResolver(cfg, store)

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ServerHeader:          "ccgp",
		AppName:               "ccgp v" + ProxyVersion,
	})

	// Middleware. No CORS: browsers must not reach the proxy.
	app.Use(recover.New())

	// Enable HTTP logging only when simple log mode is enabled
	if cfg.SimpleLog {
		app.Use(func(c *fiber.Ctx) error {
			start := time.Now()
			err := c.Next()
			profile := ""
			if rc := requestConfig(c); rc != nil {
				profile = " [" + rc.Profile + "]"
			}
			fmt.Printf("[%s] %d - %s %s %s%s\n", start.Format("15:04:05"),
				c.Response().StatusCode(), time.Since(start), c.Method(), maskToken(c.Path()), profile)
			return err
		})
	}

	// Health check endpoint; pid lets `ccgp stop` verify the process.
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"status":  "ok",
			"service": "ccgp",
			"version": ProxyVersion,
			"pid":     os.Getpid(),
		})
	})

	auth := requireProfile(index, build)

	// Proxy info (token-protected: exposes routing config)
	app.Get(callPrefix+":token/", auth, func(c *fiber.Ctx) error {
		rc := requestConfig(c)
		return c.JSON(fiber.Map{
			"version":  ProxyVersion,
			"profile":  rc.Profile,
			"provider": rc.ProviderName,
			"type":     rc.ProviderType,
			"models":   rc.ModelRules,
			"default":  rc.DefaultModel,
		})
	})

	// Claude API endpoints
	setupClaudeEndpoints(app, auth, &sessions{store: store})

	// Graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		fmt.Println("\n🛑 Shutting down...")
		_ = app.Shutdown()
	}()

	addr := fmt.Sprintf("%s:%s", cfg.Host, cfg.Port)
	fmt.Printf("✅ ccgp listening on http://%s%s<token>\n", addr, callPrefix)
	for name, p := range cfg.Profiles {
		fmt.Printf("   profile %s → provider %s (%s)\n", name, p.Provider, cfg.Providers[p.Provider].Type)
	}
	if cfg.IsEnvMode() {
		fmt.Printf("   env-only mode: routing %s\n", getRoutingMode(cfg))
	}

	return app.Listen(addr)
}

// profileResolver builds the token index and per-profile config factory.
func profileResolver(cfg *config.Config, store creds.Store) (*creds.TokenIndex, profileBuilder) {
	if cfg.IsEnvMode() {
		// Single env token; base config already holds env provider + tier overrides.
		return creds.StaticIndex(config.EnvProfile, cfg.Token),
			func(string) (*config.Config, error) { return cfg, nil }
	}
	src := newCfgSource(cfg)
	build := func(profile string) (*config.Config, error) {
		var err error
		cfg := src.get()
		pf, ok := cfg.Profiles[profile]
		if !ok {
			// Token exists in creds but profile was removed from config.
			return nil, fmt.Errorf("profile %q is not configured", profile)
		}
		pr := cfg.Providers[pf.Provider]
		key := ""
		if pr.Type == config.ProviderTypeCodexAPI {
			if pr.APIKeyEnv != "" {
				key = os.Getenv(pr.APIKeyEnv)
			} else if key, err = store.APIKey(pr.Name); err != nil {
				return nil, err
			}
			if key == "" && !isLocalURL(pr.BaseURL) {
				return nil, fmt.Errorf("provider %q has no API key, run `ccgp login %s`", pr.Name, pr.Name)
			}
			if key == "" {
				key = "ollama"
			}
		}
		return cfg.ForProfile(profile, key)
	}
	return &creds.TokenIndex{Store: store}, build
}

func isLocalURL(u string) bool {
	c := &config.Config{OpenAIBaseURL: u}
	return c.IsLocalhost()
}

func getRoutingMode(cfg *config.Config) string {
	if cfg.OpusModel != "" || cfg.SonnetModel != "" || cfg.HaikuModel != "" {
		return "custom (env overrides)"
	}
	return "pattern-based (" + converter.DefaultOpusModel + " / " + converter.DefaultHaikuModel + ")"
}

func setupClaudeEndpoints(app *fiber.App, auth fiber.Handler, ss *sessions) {
	// Messages endpoint - main Claude API
	app.Post(callPrefix+":token/v1/messages", auth, func(c *fiber.Ctx) error {
		cfg := requestConfig(c)
		if cfg.ProviderType == config.ProviderTypeCodex {
			return handleCodexMessages(c, cfg, ss.get(cfg.ProviderName))
		}
		return handleMessages(c, cfg)
	})

	// Token counting endpoint
	app.Post(callPrefix+":token/v1/messages/count_tokens", auth, func(c *fiber.Ctx) error {
		return handleCountTokens(c, requestConfig(c))
	})
}
