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
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/angel"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/converter"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/chat"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/codexh"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/codexh/wsh"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/obs"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/rt"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
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
		_, _ = fmt.Fprintf(shared.LogOut, "[%s] %d - rejected %s %s: %s (content-length %d%s)\n", time.Now().Format("15:04:05"),
			fe.Code, c.Method(), shared.MaskToken(c.Path()), fe.Message, c.Request().Header.ContentLength(), hint)
	}
	return fiber.DefaultErrorHandler(c, err)
}

// registerCallRoutes mounts every token-protected endpoint under /call/:token/.
func registerCallRoutes(app *fiber.App, auth fiber.Handler, s *shared.Sessions) {
	// Proxy info (token-protected: exposes routing config)
	app.Get(shared.CallPrefix+":token/", auth, func(c *fiber.Ctx) error {
		rc := shared.RequestConfig(c)
		return c.JSON(fiber.Map{
			"version":  ProxyVersion,
			"profile":  rc.Profile,
			"provider": rc.ProviderName,
			"type":     rc.ProviderType,
			"models":   rc.ModelRules,
			"default":  rc.DefaultModel,
		})
	})

	app.Get(shared.CallPrefix+":token/model", auth, chat.ModelRoute)

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

	shared.LogOut = obs.NewDayWriter(os.Stdout)
	app := fiber.New(appConfig(cfg.MaxBodyBytes()))

	// Middleware. No CORS: browsers must not reach the proxy.
	app.Use(recover.New())
	flight := &rt.InFlight{}
	app.Use(flight.Middleware())

	// Enable HTTP logging only when simple log mode is enabled
	if cfg.SimpleLog {
		app.Use(rt.AccessLog(shared.LogOut))
	}

	// Health check endpoint; pid lets `ccgp --stop` verify the process,
	// exe/exe_mod let `ccgp` detect a server started from an older binary.
	exe, exeMod := exeInfo()
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(healthPayload(flight, exe, exeMod))
	})

	registerCallRoutes(app, shared.RequireProfile(index, build), shared.NewSessions(store))

	// Graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		fmt.Println("\n🛑 Shutting down...")
		_ = app.Shutdown()
	}()

	addr := fmt.Sprintf("%s:%s", cfg.Host, cfg.Port)
	fmt.Printf("✅ ccgp listening on http://%s%s<token>\n", addr, shared.CallPrefix)
	for name, p := range cfg.Profiles {
		fmt.Printf("   profile %s → provider %s (%s)\n", name, p.Provider, cfg.Providers[p.Provider].Type)
	}
	if cfg.IsEnvMode() {
		fmt.Printf("   env-only mode: routing %s\n", getRoutingMode(cfg))
	}
	active, total := applyConnectionLimits(cfg)
	fmt.Printf("   upstream connections: %d active, %d total\n", active, total)
	for _, l := range peakBanner(cfg) {
		fmt.Println(l)
	}
	if cfg.ConnectionsProxy != "" {
		fmt.Printf("   upstream proxy: %s\n", proxyText(cfg))
	}
	go logUpstreamStats(shared.LogOut)

	// Once the port is ours: the record of this server, its crash file, and a word
	// about a previous server that vanished (angel.BeginRun).
	var life *angel.Lifecycle
	app.Hooks().OnListen(func(fiber.ListenData) error {
		var note string
		life, note = angel.BeginRun(ProxyVersion)
		if note != "" {
			_, _ = fmt.Fprintf(shared.LogOut, "[%s] %s\n", time.Now().Format("15:04:05"), note)
		}
		if life != nil && life.Run.Job != "none" {
			_, _ = fmt.Fprintf(shared.LogOut, "   server process: pid %s, started by pid %s, in a job: %s\n",
				life.Run.PID, life.Run.Parent, life.Run.Job)
		}
		return nil
	})
	err = app.Listen(addr)
	life.End()
	return err
}

// profileResolver builds the token index and per-profile config factory.
func profileResolver(cfg *config.Config, store creds.Store) (*creds.TokenIndex, shared.ProfileBuilder) {
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

func setupClaudeEndpoints(app *fiber.App, auth fiber.Handler, ss *shared.Sessions) {
	// Messages endpoint - main Claude API
	app.Post(shared.CallPrefix+":token/v1/messages", auth, func(c *fiber.Ctx) error {
		cfg := shared.RequestConfig(c)
		if msg := peakRefusal(cfg, ss.Pauses(), time.Now()); msg != "" {
			return shared.AnthropicError(c, fiber.StatusBadRequest, "invalid_request_error", msg)
		}
		switch cfg.ProviderType {
		case config.ProviderTypeCodex:
			return codexh.HandleCodexMessages(c, cfg, ss.Get(cfg.ProviderName))
		case config.ProviderTypeAnthropicCompat:
			return chat.HandleAnthropicCompat(c, cfg)
		}
		return chat.HandleMessages(c, cfg)
	})

	// Token counting endpoint
	app.Post(shared.CallPrefix+":token/v1/messages/count_tokens", auth, func(c *fiber.Ctx) error {
		cfg := shared.RequestConfig(c)
		if cfg.ProviderType == config.ProviderTypeAnthropicCompat {
			return chat.HandleAnthropicCountTokens(c, cfg)
		}
		return chat.HandleCountTokens(c, cfg)
	})
}

// exeInfo captures the running binary's path and modification time; called
// once at server start so /health reports the code the server runs.
func exeInfo() (path string, mod int64) {
	path, err := os.Executable()
	if err != nil {
		return "", 0
	}
	if fi, err := os.Stat(path); err == nil {
		mod = fi.ModTime().UnixNano()
	}
	return path, mod
}

// healthPayload is the /health body. exe/exe_mod let the CLI detect a server
// started from an older binary; active reports in-flight model requests.
func healthPayload(f *rt.InFlight, exe string, exeMod int64) fiber.Map {
	return fiber.Map{
		"status":  "ok",
		"service": "ccgp",
		"version": ProxyVersion,
		"pid":     os.Getpid(),
		"exe":     exe,
		"exe_mod": exeMod,
		"active":  f.Count(),
	}
}

// cfgSource reloads config.ktav when it changes, so profiles and mappings
// edited while the server runs apply without restart, connection limits
// included. Listen address changes still need a restart.
type cfgSource struct {
	mu  sync.Mutex
	cfg *config.Config
	mod time.Time
}

func newCfgSource(cfg *config.Config) *cfgSource {
	s := &cfgSource{cfg: cfg}
	if st, err := os.Stat(cfg.ConfigFile); err == nil {
		s.mod = st.ModTime()
	}
	return s
}

func (s *cfgSource) get() *config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.IsEnvMode() {
		return s.cfg
	}
	st, err := os.Stat(s.cfg.ConfigFile)
	if err != nil || st.ModTime().Equal(s.mod) {
		return s.cfg
	}
	next, err := config.Load()
	if err != nil {
		fmt.Printf("[WARN] config reload failed, keeping previous: %v\n", err)
		s.mod = st.ModTime() // don't retry until the file changes again
		return s.cfg
	}
	next.Debug, next.SimpleLog = s.cfg.Debug, s.cfg.SimpleLog
	next.Host, next.Port = s.cfg.Host, s.cfg.Port
	reapplyConnectionLimits(s.cfg, next)
	s.cfg, s.mod = next, st.ModTime()
	return s.cfg
}

// applyConnectionLimits sizes the connection pools of every upstream client
// from connections.active / .total; it returns the values applied.
func applyConnectionLimits(cfg *config.Config) (active, total int) {
	active, total = cfg.ConnectionLimits()
	for _, rt := range []http.RoundTripper{codex.HTTPClient.Transport, chat.UpstreamClient.Transport, rt.OpenAITransport} {
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
		"codex": codex.HTTPClient.Transport, "anthropic": chat.UpstreamClient.Transport, "openai": rt.OpenAITransport,
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
		wsh.WriteCodexWebsocketStats(w, now)
		obs.WriteLinkStats(w, now)
		obs.WriteSpeedStats(w, now)
		obs.WriteCacheStats(w, now)
	}
}
