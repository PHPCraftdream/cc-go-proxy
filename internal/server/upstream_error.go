package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
)

// maxUpstreamMsg caps the message of an upstream refusal in a log line.
const maxUpstreamMsg = 200

// upstreamErrorText is why an Anthropic-style endpoint refused a request, for the
// log: the error's type and code with its message, then, for a rate limit or an
// overload, the limit headers, and the request ids. A key the message may echo
// is hidden. The client gets the reply as it came.
func upstreamErrorText(status int, h http.Header, body []byte) string {
	label, msg := upstreamErrorParts(body)
	text := msg
	if label != "" {
		text = label + ": " + msg
	}
	limits := ""
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		limits = rateHeaders(h)
	}
	return joinNonEmpty(oneLine(redactSecrets(text), maxUpstreamMsg), limits, upstreamIDs(h))
}

// upstreamErrorParts reads the error's label (its type and code, as "rate_limit_error 1302")
// and message out of the shapes the endpoints use: {"type":"error","error":{"type","message"}},
// {"error":{"code","message"}} and {"code":…,"msg":…}. A body that is not JSON (an edge's
// HTML page, plain text) gives no label and codex.ErrorMessage's line.
func upstreamErrorParts(body []byte) (label, msg string) {
	var top struct {
		Type    string `json:"type"`
		Code    any    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
		Detail  string `json:"detail"`
		Error   any    `json:"error"`
	}
	if json.Unmarshal(body, &top) != nil {
		return "", codex.ErrorMessage(body)
	}
	typ, code, msg := top.Type, top.Code, firstNonEmpty(top.Message, top.Msg, top.Detail)
	switch e := top.Error.(type) {
	case string:
		msg = firstNonEmpty(e, msg)
	case map[string]any:
		if s, _ := e["type"].(string); s != "" {
			typ = s
		}
		if e["code"] != nil {
			code = e["code"]
		}
		if s, _ := e["message"].(string); s != "" {
			msg = s
		}
	}
	if typ == "error" { // the envelope's own type says nothing
		typ = ""
	}
	var parts []string
	if typ != "" {
		parts = append(parts, typ)
	}
	if c := codeText(code); c != "" {
		parts = append(parts, c)
	}
	if msg == "" {
		msg = codex.ErrorMessage(body)
	}
	return strings.Join(parts, " "), msg
}

// codeText prints an error code, a number or a string, without exponent noise.
func codeText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case float64:
		return fmt.Sprintf("%.0f", c)
	}
	return ""
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// secretPatterns are what a refusal may echo of the credential it was given.
var secretPatterns = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`sk-[A-Za-z0-9_\-]{6,}`), "sk-***"},
	{regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-+/=]{6,}`), "Bearer ***"},
	{regexp.MustCompile(`[0-9a-f]{32}\.[A-Za-z0-9]{8,}`), "***"},
	{regexp.MustCompile(`(?i)((?:api[_-]?key|token|authorization|secret)["']?\s*[:=]\s*["']?)[^\s"',;]{6,}`), "${1}***"},
}

// redactSecrets hides keys and tokens in a text that goes to the log.
func redactSecrets(s string) string {
	for _, p := range secretPatterns {
		s = p.re.ReplaceAllString(s, p.with)
	}
	return s
}
