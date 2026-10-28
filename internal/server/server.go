// Package server implements the HTTP proxy server that translates between
// Claude API format and upstream providers (OpenAI-compatible, Codex OAuth).
//
// Requests arrive on /call/{token}/v1/messages; the token selects a profile,
// which selects the provider and model mapping.
package server

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/converter"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

// ProxyVersion is the version of the proxy. Release builds set it from the
// tag: -ldflags "-X github.com/PHPCraftdream/cc-go-proxy/internal/server.ProxyVersion=1.2.3".
var ProxyVersion = "0.1.0"

// appConfig sets the request body cap from listen.max_body_mb: a long Claude
// Code conversation resends its whole history on every call, so fiber's 4 MiB
// default cuts live sessions off with a connection reset.
func appConfig(maxBody int) fiber.Config {
	return fiber.Config{
		DisableStartupMessage: true,
		ServerHeader:          "ccgp",
		AppName:               "ccgp v" + ProxyVersion,
		BodyLimit:             maxBody,
		ErrorHandler:          errorHandler,
	}
}

// errorHandler logs requests that fasthttp refuses before any route runs
// (oversized body or headers); they never reach the access log.
func errorHandler(c *fiber.Ctx, err error) error {
	var fe *fiber.Error
	if errors.As(err, &fe) && (fe.Code == fiber.StatusRequestEntityTooLarge || fe.Code == fiber.StatusRequestHeaderFieldsTooLarge) {
		hint := ""
		if fe.Code == fiber.StatusRequestEntityTooLarge {
			hint = "; raise listen.max_body_mb"
		}
		_, _ = fmt.Fprintf(logOut, "[%s] %d - rejected %s %s: %s (content-length %d%s)\n", time.Now().Format("15:04:05"),
			fe.Code, c.Method(), maskToken(c.Path()), fe.Message, c.Request().Header.ContentLength(), hint)
	}
	return fiber.DefaultErrorHandler(c, err)
}

// maxLogErr caps the error text of a log line.
const maxLogErr = 300

// oneLine flattens s to a single line of at most n bytes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n], "") + "…"
	}
	return s
}

// accessLog prints one line per request: start time, status, duration, route,
// and for failures (status >= 400) the error message.
func accessLog(w io.Writer) fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()
		profile := ""
		if rc := requestConfig(c); rc != nil {
			profile = " [" + rc.Profile + "]"
		}
		if m, _ := c.Locals(localsModel).(string); m != "" {
			profile += " " + m
		}
		status := c.Response().StatusCode()
		if m, _ := c.Locals(localsErr).(string); m != "" && status >= 400 {
			profile += ": " + oneLine(m, maxLogErr)
		}
		if m, _ := c.Locals(localsConn).(string); m != "" {
			profile += " | " + m
		}
		_, _ = fmt.Fprintf(w, "[%s] %d - %s %s %s%s\n", start.Format("15:04:05"),
			status, time.Since(start), c.Method(), maskToken(c.Path()), profile)
		return err
	}
}

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

// registerCallRoutes mounts every token-protected endpoint under /call/:token/.
func registerCallRoutes(app *fiber.App, auth fiber.Handler, s *sessions) {
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

	app.Get(callPrefix+":token/model", auth, modelRoute)

	// Claude API endpoints
	setupClaudeEndpoints(app, auth, s)
}

// Start initializes and starts the HTTP server (blocks).
func Start(cfg *config.Config) error {
	dir, err := creds.DefaultDir()
	if err != nil {
		return err
	}
	store := creds.Store{Dir: dir}
	index, build := profileResolver(cfg, store)

	app := fiber.New(appConfig(cfg.MaxBodyBytes()))

	// Middleware. No CORS: browsers must not reach the proxy.
	app.Use(recover.New())
	flight := &inFlight{}
	app.Use(flight.middleware())

	// Enable HTTP logging only when simple log mode is enabled
	if cfg.SimpleLog {
		app.Use(accessLog(os.Stdout))
	}

	// Health check endpoint; pid lets `ccgp --stop` verify the process,
	// exe/exe_mod let `ccgp` detect a server started from an older binary.
	exe, exeMod := exeInfo()
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(healthPayload(flight, exe, exeMod))
	})

	registerCallRoutes(app, requireProfile(index, build), &sessions{store: store})

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
	active, total := applyConnectionLimits(cfg)
	fmt.Printf("   upstream connections: %d active, %d total\n", active, total)
	if cfg.ConnectionsProxy != "" {
		fmt.Printf("   upstream proxy: %s\n", proxyText(cfg))
	}
	go logUpstreamStats(logOut)

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
		if pr.Type == config.ProviderTypeCodexAPI || pr.Type == config.ProviderTypeAnthropicCompat {
			if pr.APIKeyEnv != "" {
				key = os.Getenv(pr.APIKeyEnv)
			} else if key, err = store.APIKey(pr.Name); err != nil {
				return nil, err
			}
			if key == "" && !isLocalURL(pr.BaseURL) {
				return nil, fmt.Errorf("provider %q has no API key, run `ccgp --login %s`", pr.Name, pr.Name)
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
		switch cfg.ProviderType {
		case config.ProviderTypeCodex:
			return handleCodexMessages(c, cfg, ss.get(cfg.ProviderName))
		case config.ProviderTypeAnthropicCompat:
			return handleAnthropicCompat(c, cfg)
		}
		return handleMessages(c, cfg)
	})

	// Token counting endpoint
	app.Post(callPrefix+":token/v1/messages/count_tokens", auth, func(c *fiber.Ctx) error {
		cfg := requestConfig(c)
		if cfg.ProviderType == config.ProviderTypeAnthropicCompat {
			return handleAnthropicCountTokens(c, cfg)
		}
		return handleCountTokens(c, cfg)
	})
}
