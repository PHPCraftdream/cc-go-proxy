package rt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
	"github.com/PHPCraftdream/cc-go-proxy/pkg/models"
	"github.com/gofiber/fiber/v2"
)

// localsActiveGuard holds the per-request release guard under /call/.
const localsActiveGuard = "ccgp:active-guard"

// inFlight counts the requests currently being served under /call/.
type InFlight struct{ n atomic.Int64 }

// activeGuard owns the single decrement of one counted request: release is
// safe to call any number of times, from the middleware after the handler
// returned or from a stream writer that took the release over.
type activeGuard struct {
	f     *InFlight
	once  sync.Once
	taken bool // a stream writer owns the release; middleware must not fire
}

func (g *activeGuard) release() { g.once.Do(func() { g.f.dec() }) }

// middleware counts requests under /call/ (model endpoints); /health itself
// must not count. Streaming handlers keep counting until the stream ends:
// deferActive moves the release into the stream writer; otherwise the count
// would drop when the handler returns, while the response still streams.
func (f *InFlight) Middleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if !strings.HasPrefix(c.Path(), shared.CallPrefix) {
			return c.Next()
		}
		f.n.Add(1)
		g := &activeGuard{f: f}
		c.Locals(localsActiveGuard, g)
		// Deferred so a panic unwinding to the recover middleware still releases.
		defer func() {
			if !g.taken {
				g.release()
			}
		}()
		return c.Next()
	}
}

// deferActive moves the in-flight release of the current request into the
// caller: the returned func decrements the counter exactly once. Call it
// right before installing a body stream writer and defer the result as the
// first statement inside the writer closure (fiber reuses the context after
// the handler returns, so the closure must not touch c).
func DeferActive(c *fiber.Ctx) (release func()) {
	g, ok := c.Locals(localsActiveGuard).(*activeGuard)
	if !ok {
		return func() {}
	}
	g.taken = true
	return g.release
}

func (f *InFlight) dec() { f.n.Add(-1) }

func (f *InFlight) Count() int64 { return f.n.Load() }

// openAITransport carries the OpenAI-compatible requests.
var OpenAITransport = connpool.NewRouter(0, 0, 0, nil)

// clientWatchTick is how often the watch looks at the client's connection.
const ClientWatchTick = 500 * time.Millisecond

// watchClientGone calls gone once the client closes conn while a request is
// handled. It reads the connection, idle meanwhile: Claude Code sends nothing
// before the reply (no pipelining), and the reply closes the connection, so
// nothing a later request would need is taken. Only a TCP connection is
// watched (tests serve requests over in-memory ones). stop ends the watch and
// leaves the connection without a read deadline.
func WatchClientGone(conn net.Conn, gone func()) (stop func()) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return func() {}
	}
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var b [1]byte
		for {
			select {
			case <-quit:
				return
			default:
			}
			_ = tcp.SetReadDeadline(time.Now().Add(ClientWatchTick))
			n, err := tcp.Read(b[:])
			switch {
			case n > 0:
				return // data, not a close: nothing more to tell
			case err == nil, errors.Is(err, os.ErrDeadlineExceeded):
				continue
			default:
				select {
				case <-quit: // the deadline set by stop
				default:
					gone()
				}
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(quit)
			_ = tcp.SetReadDeadline(time.Now())
			<-done
			_ = tcp.SetReadDeadline(time.Time{})
		})
	}
}

// logUpstreamRetry prints the line for a stream sent again after an overload or
// a broken connection; attempt counts of max.
func LogUpstreamRetry(w io.Writer, profile, model string, attempt, max int, wait time.Duration, reason, detail, ids string) {
	line := shared.EventText(reason, detail)
	_, _ = fmt.Fprintf(w, "[%s] upstream retry %d/%d in %s [%s] %s: %s\n", time.Now().Format("15:04:05"),
		attempt, max, wait, profile, model, shared.JoinNonEmpty(line, ids))
}

// logStreamCanceled prints the line for a stream the client left.
func LogStreamCanceled(w io.Writer, started time.Time, what, profile, model, extra string) {
	_, _ = fmt.Fprintf(w, "[%s] %s canceled by client after %s [%s] %s: %s\n", started.Format("15:04:05"), what,
		time.Since(started).Round(time.Millisecond), profile, model, extra)
}

// logSlowStream prints the timing line of a stream that completed slowly.
func LogSlowStream(w io.Writer, started time.Time, what, profile, model, summary string) {
	_, _ = fmt.Fprintf(w, "[%s] slow %s %s [%s] %s: %s\n", started.Format("15:04:05"), what,
		time.Since(started).Round(time.Millisecond), profile, model, summary)
}

// upstreamCtx puts the profile's way out (its own proxy, none, or the global
// one; see config.ProxyChoice) on the context of an upstream request. The
// clients' connpool.Router sends the request over the pools of that choice.
func UpstreamCtx(ctx context.Context, cfg *config.Config) context.Context {
	return codex.WithClientVersion(proxydial.WithChoice(ctx, cfg.ProxyChoice(cfg.Profile)), cfg.CodexClientVersion())
}

// addOpenRouterHeaders adds OpenRouter-specific HTTP headers for better rate limits.
// Sets HTTP-Referer and X-Title headers when configured, which helps with OpenRouter's
// rate limiting and usage tracking.
func AddOpenRouterHeaders(req *http.Request, cfg *config.Config) {
	if cfg.OpenRouterAppURL != "" {
		req.Header.Set("HTTP-Referer", cfg.OpenRouterAppURL)
	}
	if cfg.OpenRouterAppName != "" {
		req.Header.Set("X-Title", cfg.OpenRouterAppName)
	}
}

// writeSSEEvent writes a Server-Sent Event.
func WriteSSEEvent(w *bufio.Writer, event string, data interface{}) error {
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", string(dataJSON))
	return err
}

// callOpenAIInternal is the internal implementation without retry logic
func CallOpenAIInternal(req *models.OpenAIRequest, cfg *config.Config) (*models.OpenAIResponse, error) {
	// Marshal request to JSON
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Build API URL
	apiURL := cfg.OpenAIBaseURL + "/chat/completions"

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(UpstreamCtx(context.Background(), cfg), "POST", apiURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/json")

	// Skip auth for Ollama (localhost) - Ollama doesn't require authentication
	if !cfg.IsLocalhost() {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.OpenAIAPIKey)
	}

	// OpenRouter-specific headers for better rate limits
	if cfg.DetectProvider() == config.ProviderOpenRouter {
		AddOpenRouterHeaders(httpReq, cfg)
	}

	client := &http.Client{Transport: OpenAITransport}

	// Make request with a deadline that remains active while the body is read.
	resp, err := DoOpenAIRequest(UpstreamCtx(context.Background(), cfg), client, httpReq, 90*time.Second)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Check for errors
	if resp.StatusCode != http.StatusOK {
		return nil, NewOpenAIUpstreamError(resp.StatusCode, resp.Header, respBody)
	}

	// Parse response
	var openaiResp models.OpenAIResponse
	if err := json.Unmarshal(respBody, &openaiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &openaiResp, nil
}

func DoOpenAIRequest(ctx context.Context, client *http.Client, req *http.Request, timeout time.Duration) (*http.Response, error) {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	resp, err := client.Do(req.WithContext(requestCtx))
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelReadCloser{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelReadCloser) Close() error {
	b.cancel()
	return b.ReadCloser.Close()
}

type OpenAIUpstreamError struct {
	StatusCode int
	Message    string
	Headers    http.Header
}

func (e *OpenAIUpstreamError) Error() string {
	return fmt.Sprintf("upstream returned status %d: %s", e.StatusCode, e.Message)
}

func NewOpenAIUpstreamError(status int, headers http.Header, body []byte) *OpenAIUpstreamError {
	forward := make(http.Header)
	for key, values := range headers {
		lower := strings.ToLower(key)
		if lower == "retry-after" || strings.HasPrefix(lower, "x-ratelimit-") {
			forward[key] = append([]string(nil), values...)
		}
	}
	return &OpenAIUpstreamError{
		StatusCode: status,
		Message:    codex.ErrorMessage(body),
		Headers:    forward,
	}
}

// quietRoute reports a request that says nothing when it succeeds: the model
// lookup and the health check, which Claude Code and ccgp repeat all day.
func quietRoute(c *fiber.Ctx) bool {
	return c.Method() == fiber.MethodGet && (c.Path() == "/health" || strings.HasSuffix(c.Path(), "/model"))
}

// accessLog prints one line per request: start time, status, duration, route,
// and for failures (status >= 400) the error message. A successful quietRoute
// request gets no line.
func AccessLog(w io.Writer) fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()
		if status := c.Response().StatusCode(); status < 400 && quietRoute(c) {
			return err
		}
		profile := ""
		if rc := shared.RequestConfig(c); rc != nil {
			profile = " [" + rc.Profile + "]"
		}
		if m, _ := c.Locals(shared.LocalsModel).(string); m != "" {
			profile += " " + m
		}
		status := c.Response().StatusCode()
		if m, _ := c.Locals(shared.LocalsErr).(string); m != "" && status >= 400 {
			profile += ": " + shared.OneLine(m, shared.MaxLogErr)
		}
		if m, _ := c.Locals(shared.LocalsConn).(string); m != "" {
			profile += " | " + m
		}
		_, _ = fmt.Fprintf(w, "[%s] %d - %s %s %s%s\n", start.Format("15:04:05"),
			status, time.Since(start), c.Method(), shared.MaskToken(c.Path()), profile)
		return err
	}
}
