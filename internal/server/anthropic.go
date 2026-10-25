package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/converter"
	"github.com/PHPCraftdream/cc-go-proxy/internal/identity"
	"github.com/PHPCraftdream/cc-go-proxy/internal/preset"
	"github.com/gofiber/fiber/v2"
)

// upstreamClient has no overall timeout: responses stream for minutes.
var upstreamClient = &http.Client{}

// forwardedResponseHeaders are copied from the upstream reply to the client.
var forwardedResponseHeaders = []string{
	"Content-Type", "Retry-After", "Request-Id", "X-Request-Id", "Cache-Control",
}

// rewriteAnthropicBody maps the model, converts the effort level through
// efforts and corrects the model identity in a Messages request, leaving every
// other field as Claude Code sent it.
func rewriteAnthropicBody(body []byte, cfg *config.Config, efforts map[string]string) (out []byte, requested, backend string, err error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, "", "", fmt.Errorf("invalid request body: %w", err)
	}
	requested, _ = m["model"].(string)
	backend = converter.MapModel(requested, cfg)
	m["model"] = backend
	mapEffort(m, efforts)

	if sys, ok := m["system"]; ok {
		m["system"] = identity.RewriteContent(sys, requested, backend)
	}
	if msgs, ok := m["messages"].([]any); ok {
		for _, raw := range msgs {
			msg, ok := raw.(map[string]any)
			if !ok || msg["role"] == "assistant" {
				continue
			}
			msg["content"] = identity.RewriteContent(msg["content"], requested, backend)
		}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, "", "", err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), requested, backend, nil
}

// handleAnthropicCompat serves /v1/messages for providers that speak the
// Anthropic Messages API: the request is forwarded as-is (model mapped, identity
// corrected) and the reply, including its SSE stream, is passed through.
func handleAnthropicCompat(c *fiber.Ctx, cfg *config.Config) error {
	return forwardAnthropic(c, cfg, "/v1/messages", false)
}

// handleAnthropicCountTokens forwards token counting; when the upstream has no
// such endpoint a length-based estimate keeps Claude Code working.
func handleAnthropicCountTokens(c *fiber.Ctx, cfg *config.Config) error {
	return forwardAnthropic(c, cfg, "/v1/messages/count_tokens", true)
}

func forwardAnthropic(c *fiber.Ctx, cfg *config.Config, path string, estimateOnFailure bool) error {
	pr := cfg.Providers[cfg.ProviderName]
	if pr == nil || pr.BaseURL == "" {
		return anthropicError(c, fiber.StatusServiceUnavailable, "api_error", "provider has no base_url")
	}
	body, requested, backend, err := rewriteAnthropicBody(c.Body(), cfg, effortMapFor(pr))
	if err != nil {
		return anthropicError(c, fiber.StatusBadRequest, "invalid_request_error", err.Error())
	}
	if cfg.Debug {
		fmt.Printf("\n=== ANTHROPIC-COMPAT REQUEST (%s → %s) ===\n%s\n", requested, backend, body)
	}
	c.Locals(localsModel, requested+" -> "+backend)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(pr.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		cancel()
		return anthropicError(c, fiber.StatusBadGateway, "api_error", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.OpenAIAPIKey)
	req.Header.Set("User-Agent", "ccgp")
	version := c.Get("anthropic-version")
	if version == "" {
		version = "2023-06-01"
	}
	req.Header.Set("anthropic-version", version)
	if beta := c.Get("anthropic-beta"); beta != "" {
		req.Header.Set("anthropic-beta", beta)
	}
	if accept := c.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}

	resp, err := upstreamClient.Do(req)
	if err != nil {
		cancel()
		if estimateOnFailure {
			return c.JSON(fiber.Map{"input_tokens": estimateTokens(body)})
		}
		return anthropicError(c, fiber.StatusBadGateway, "api_error", "upstream request failed: "+err.Error())
	}

	if estimateOnFailure && resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		cancel()
		return c.JSON(fiber.Map{"input_tokens": estimateTokens(body)})
	}

	for _, h := range forwardedResponseHeaders {
		if v := resp.Header.Get(h); v != "" {
			c.Set(h, v)
		}
	}
	c.Status(resp.StatusCode)

	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		c.Set("Cache-Control", "no-cache")
		c.Set("X-Accel-Buffering", "no")
		release := deferActive(c)
		c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
			defer release()
			defer cancel()
			defer func() { _ = resp.Body.Close() }()
			buf := make([]byte, 32<<10)
			for {
				n, rerr := resp.Body.Read(buf)
				if n > 0 {
					if _, werr := w.Write(buf[:n]); werr != nil || w.Flush() != nil {
						return // client gone: abort upstream
					}
				}
				if rerr != nil {
					return
				}
			}
		})
		return nil
	}

	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return anthropicError(c, fiber.StatusBadGateway, "api_error", "reading upstream reply: "+err.Error())
	}
	return c.Send(b)
}

// estimateTokens is a rough length-based count (about four bytes per token).
func estimateTokens(body []byte) int {
	n := len(body) / 4
	if n < 1 {
		n = 1
	}
	return n
}

// effortMapFor is the provider's own effort_map, else its preset's.
func effortMapFor(pr *config.Provider) map[string]string {
	if len(pr.EffortMap) > 0 {
		return pr.EffortMap
	}
	if p, ok := preset.ForBaseURL(pr.BaseURL); ok {
		return p.EffortMap
	}
	return nil
}

// mapEffort converts output_config.effort in place; levels not in the map and
// requests without an effort are left alone.
func mapEffort(m map[string]any, efforts map[string]string) {
	if len(efforts) == 0 {
		return
	}
	oc, ok := m["output_config"].(map[string]any)
	if !ok {
		return
	}
	level, ok := oc["effort"].(string)
	if !ok {
		return
	}
	if to, ok := efforts[strings.ToLower(level)]; ok && to != "" {
		oc["effort"] = to
	}
}
