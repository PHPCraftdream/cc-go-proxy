package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
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

// logStreamFailure prints the line for a stream that broke after its reply started.
func logStreamFailure(w io.Writer, started time.Time, profile, model, msg string) {
	_, _ = fmt.Fprintf(w, "[%s] stream failed after %s [%s] %s: %s\n", started.Format("15:04:05"),
		time.Since(started).Round(time.Millisecond), profile, model, oneLine(msg, maxLogErr))
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
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := codex.Call(ctx, baseURL, s, req)
	if err != nil {
		cancel()
		if errors.Is(err, oauth.ErrNotLoggedIn) || errors.Is(err, oauth.ErrReloginRequired) {
			return anthropicError(c, fiber.StatusUnauthorized, "authentication_error",
				fmt.Sprintf("%v (ccgp --login %s)", err, cfg.ProviderName))
		}
		return anthropicError(c, fiber.StatusBadGateway, "api_error", err.Error())
	}
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
		return anthropicError(c, status, codex.ErrorType(status), "codex: "+codex.ErrorMessage(b))
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
	profile, mapped, started := cfg.Profile, probe.Model+" -> "+model, time.Now()
	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
		tr := &codex.Translator{Model: probe.Model, Emit: func(ev string, data any) {
			writeSSEEvent(w, ev, data)
			if err := w.Flush(); err != nil {
				cancel() // client gone: abort upstream
			}
		}}
		_ = tr.Run(resp.Body)
		if tr.Failed != "" {
			logStreamFailure(logOut, started, profile, mapped, tr.Failed)
		}
	})
	return nil
}
