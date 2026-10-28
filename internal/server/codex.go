package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/converter"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/gofiber/fiber/v2"
)

func anthropicError(c *fiber.Ctx, status int, typ, msg string) error {
	c.Locals(localsErr, msg)
	return c.Status(status).JSON(fiber.Map{
		"type":  "error",
		"error": fiber.Map{"type": typ, "message": msg},
	})
}

// logOut receives the non-access log lines (rejected bodies, broken streams).
var logOut io.Writer = os.Stdout

// maxLogDetail caps the raw upstream event in a stream failure line.
const maxLogDetail = 1000

// logStreamFailure prints the line for a stream that broke after its reply
// started; detail is the raw upstream event that ended it, extra the request
// ids and timing, either may be empty.
func logStreamFailure(w io.Writer, started time.Time, profile, model, msg, detail, extra string) {
	line := oneLine(msg, maxLogErr)
	if detail != "" {
		line += " | upstream event: " + oneLine(detail, maxLogDetail)
	}
	if extra != "" {
		line += " | " + extra
	}
	_, _ = fmt.Fprintf(w, "[%s] stream failed after %s [%s] %s: %s\n", started.Format("15:04:05"),
		time.Since(started).Round(time.Millisecond), profile, model, line)
}

// overloadBackoff is the pause before each retry of an overloaded backend.
var overloadBackoff = []time.Duration{2 * time.Second, 5 * time.Second}

// brokenBackoff is the pause before each retry of a stream whose connection broke
// before it sent any content. Short: the next try opens another connection.
var brokenBackoff = []time.Duration{300 * time.Millisecond, 2 * time.Second}

// callAfter waits, then calls; a non-2xx reply is an error.
func callAfter(ctx context.Context, wait time.Duration, call func() (*http.Response, error)) (*http.Response, error) {
	select {
	case <-time.After(wait):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	resp, err := call()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("retry: HTTP %d: %s", resp.StatusCode, codex.ErrorMessage(b))
	}
	return resp, nil
}

// logUpstreamRetry prints the line for a stream sent again after an overload or
// a broken connection; attempt counts of max.
func logUpstreamRetry(w io.Writer, profile, model string, attempt, max int, wait time.Duration, reason, detail, ids string) {
	line := oneLine(reason, maxLogErr)
	if detail != "" {
		line += " | upstream event: " + oneLine(detail, maxLogDetail)
	}
	_, _ = fmt.Fprintf(w, "[%s] upstream retry %d/%d in %s [%s] %s: %s\n", time.Now().Format("15:04:05"),
		attempt, max, wait, profile, model, joinNonEmpty(line, ids))
}

// logReconnect prints the line for a request ccgp sent again because its
// connection failed; the client saw nothing of it unless the retries ran out.
func logReconnect(w io.Writer, profile, model string, n int, since time.Duration, reason, conn string) {
	_, _ = fmt.Fprintf(w, "[%s] upstream reconnect %d after %s [%s] %s: %s | the failed try was on a %s connection\n",
		time.Now().Format("15:04:05"), n, since.Round(time.Millisecond), profile, model, oneLine(reason, maxLogErr), conn)
}

// logStreamCanceled prints the line for a stream the client left.
func logStreamCanceled(w io.Writer, started time.Time, profile, model, extra string) {
	_, _ = fmt.Fprintf(w, "[%s] stream canceled by client after %s [%s] %s: %s\n", started.Format("15:04:05"),
		time.Since(started).Round(time.Millisecond), profile, model, extra)
}

// logSlowStream prints the timing line of a stream that completed slowly.
func logSlowStream(w io.Writer, started time.Time, profile, model, summary string) {
	_, _ = fmt.Fprintf(w, "[%s] slow stream %s [%s] %s: %s\n", started.Format("15:04:05"),
		time.Since(started).Round(time.Millisecond), profile, model, summary)
}

// joinNonEmpty joins the non-empty parts with " | ".
func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " | ")
}

// upstreamIDs names the response's request ids, for matching a failure with
// the backend's side.
func upstreamIDs(h http.Header) string {
	var ids []string
	for _, k := range []string{"X-Request-Id", "Cf-Ray"} {
		if v := h.Get(k); v != "" {
			ids = append(ids, strings.ToLower(k)+"="+v)
		}
	}
	return strings.Join(ids, " ")
}

// handleCodexMessages serves /v1/messages via the ChatGPT Codex backend.
// The upstream call happens before streaming starts so HTTP errors (429, 401)
// reach Claude Code with their real status.
func handleCodexMessages(c *fiber.Ctx, cfg *config.Config, s *oauth.Session) error {
	body := append([]byte(nil), c.Body()...)
	if cfg.Debug {
		fmt.Printf("\n=== CLAUDE REQUEST ===\n%s\n=====================\n", body)
	}
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	model := converter.MapModel(probe.Model, cfg)
	c.Locals(localsModel, probe.Model+" -> "+model)

	req, wantStream, err := codex.ConvertRequest(body, model, cfg.Profile)
	if err != nil {
		return anthropicError(c, fiber.StatusBadRequest, "invalid_request_error", err.Error())
	}
	if cfg.Debug {
		b, _ := json.Marshal(req)
		fmt.Printf("\n=== CODEX REQUEST (%s → %s) ===\n%s\n", probe.Model, model, b)
	}

	baseURL := ""
	if pr := cfg.Providers[cfg.ProviderName]; pr != nil {
		baseURL = pr.BaseURL
	}
	ctx, cancel := context.WithCancel(upstreamCtx(context.Background(), cfg))
	trace := newUpstreamTrace()
	trace.onRetry = func(n int, err error, since time.Duration, conn string) {
		logReconnect(logOut, cfg.Profile, probe.Model+" -> "+model, n, since, err.Error(), conn)
	}
	ctx = connpool.WithObserver(httptrace.WithClientTrace(ctx, trace.clientTrace()), trace.observer())
	resp, err := codex.Call(ctx, baseURL, s, req)
	// A short rate limit is waited out here, once; a long one is Claude Code's to handle.
	if err == nil && resp.StatusCode == http.StatusTooManyRequests {
		if wait, ok := retryAfter(resp.Header, time.Now()); ok && wait <= rateRetryMax {
			logRateRetry(logOut, cfg.Profile, probe.Model+" -> "+model, wait, rateHeaders(resp.Header))
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			if sleepCtx(ctx, wait) {
				resp, err = codex.Call(ctx, baseURL, s, req)
			} else {
				err = ctx.Err()
			}
		}
	}
	if err != nil {
		cancel()
		if errors.Is(err, oauth.ErrNotLoggedIn) || errors.Is(err, oauth.ErrReloginRequired) {
			return anthropicError(c, fiber.StatusUnauthorized, "authentication_error",
				fmt.Sprintf("%v (ccgp --login %s)", err, cfg.ProviderName))
		}
		out := anthropicError(c, fiber.StatusBadGateway, "api_error", err.Error())
		if hint := trace.pingHint(err, time.Now()); hint != "" {
			c.Locals(localsErr, err.Error()+" "+hint) // the log gets the hint; the client the message
		}
		return out
	}
	c.Locals(localsConn, trace.newConnText())
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		cancel()
		status := resp.StatusCode
		if status == http.StatusUnauthorized {
			// Still 401 after refresh: credentials are no longer accepted.
			return anthropicError(c, status, "authentication_error",
				fmt.Sprintf("codex: unauthorized, run `ccgp --login %s`: %s", cfg.ProviderName, codex.ErrorMessage(b)))
		}
		msg := "codex: " + codex.ErrorMessage(b)
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			c.Set("Retry-After", ra) // Claude Code backs off by it
		}
		hdrs := rateHeaders(resp.Header)
		out := anthropicError(c, status, codex.ErrorType(status), msg)
		if status == http.StatusTooManyRequests && hdrs != "" {
			c.Locals(localsErr, msg+" | "+hdrs) // the log gets the limits; the client the message
		}
		return out
	}

	if !wantStream {
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
		col := &codex.Collector{}
		tr := &codex.Translator{Model: probe.Model, Emit: col.Emit}
		_ = tr.Run(resp.Body)
		out, err := col.Result()
		if err != nil {
			return anthropicError(c, fiber.StatusBadGateway, "api_error", err.Error())
		}
		return c.JSON(out)
	}

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")
	// The access line is printed when the handler returns, before the stream
	// ends; a failure inside the stream gets its own line.
	profile, mapped, started, bodyBytes := cfg.Profile, probe.Model+" -> "+model, time.Now(), len(body)
	release := deferActive(c)
	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		defer release()
		defer cancel()
		upstream := resp
		defer func() { _ = upstream.Body.Close() }()
		tr := &codex.Translator{Model: probe.Model, Emit: func(ev string, data any) {
			writeSSEEvent(w, ev, data)
			if err := w.Flush(); err != nil {
				cancel() // client gone: abort upstream
			}
		}}
		// An overloaded backend, or a connection that broke, before any content
		// was sent: the request is asked again after a pause; the client only
		// waits longer. Each kind has its own budget.
		overloads, breaks := 0, 0
		for {
			tr.AllowRetry = overloads < len(overloadBackoff)
			tr.AllowTransportRetry = breaks < len(brokenBackoff)
			if err := tr.Run(upstream.Body); !errors.Is(err, codex.ErrRetry) {
				break
			}
			var wait time.Duration
			var n, max int
			if tr.RetryTransport {
				wait, max = brokenBackoff[breaks], len(brokenBackoff)
				breaks++
				n = breaks
			} else {
				wait, max = overloadBackoff[overloads], len(overloadBackoff)
				overloads++
				n = overloads
			}
			logUpstreamRetry(logOut, profile, mapped, n, max, wait, tr.Retry, tr.Detail, upstreamIDs(upstream.Header))
			tr.Detail = ""
			if tr.Started() {
				tr.Emit("ping", map[string]any{"type": "ping"})
			}
			next, err := callAfter(ctx, wait, func() (*http.Response, error) { return codex.Call(ctx, baseURL, s, req) })
			if err != nil {
				tr.Fail(err.Error())
				break
			}
			_ = upstream.Body.Close()
			upstream = next
		}
		end := time.Now()
		ids := upstreamIDs(upstream.Header)
		switch {
		case tr.Failed != "" && ctx.Err() != nil:
			logStreamCanceled(logOut, started, profile, mapped, joinNonEmpty(ids, trace.summary(tr, end, bodyBytes)))
		case tr.Failed != "":
			logStreamFailure(logOut, started, profile, mapped, tr.Failed, tr.Detail,
				joinNonEmpty(ids, trace.summary(tr, end, bodyBytes)))
		case trace.slow(end):
			logSlowStream(logOut, trace.start, profile, mapped, joinNonEmpty(trace.summary(tr, end, bodyBytes), ids))
		}
	})
	return nil
}
