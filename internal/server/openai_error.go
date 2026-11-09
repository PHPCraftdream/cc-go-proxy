package server

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/gofiber/fiber/v2"
)

type openAIUpstreamError struct {
	StatusCode int
	Message    string
	Headers    http.Header
}

func (e *openAIUpstreamError) Error() string {
	return fmt.Sprintf("upstream returned status %d: %s", e.StatusCode, e.Message)
}

func newOpenAIUpstreamError(status int, headers http.Header, body []byte) *openAIUpstreamError {
	forward := make(http.Header)
	for key, values := range headers {
		lower := strings.ToLower(key)
		if lower == "retry-after" || strings.HasPrefix(lower, "x-ratelimit-") {
			forward[key] = append([]string(nil), values...)
		}
	}
	return &openAIUpstreamError{
		StatusCode: status,
		Message:    codex.ErrorMessage(body),
		Headers:    forward,
	}
}

var callTokenSegment = regexp.MustCompile(`(?i)(/call/)[^/?#\s"'<>]+`)

func redactOpenAIError(message string, cfg *config.Config) string {
	for _, secret := range []string{cfg.OpenAIAPIKey, cfg.AnthropicAPIKey} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "***")
		}
	}
	return callTokenSegment.ReplaceAllString(message, "$1***")
}

func respondOpenAIError(c *fiber.Ctx, cfg *config.Config, err error) error {
	var upstream *openAIUpstreamError
	if !errors.As(err, &upstream) {
		return anthropicError(c, http.StatusInternalServerError, "api_error", redactOpenAIError("OpenAI API error: "+err.Error(), cfg))
	}
	for key, values := range upstream.Headers {
		for _, value := range values {
			c.Append(key, value)
		}
	}
	return anthropicError(c, upstream.StatusCode, codex.ErrorType(upstream.StatusCode), redactOpenAIError(upstream.Message, cfg))
}
