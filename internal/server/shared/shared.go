package shared

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/gofiber/fiber/v2"
)

// ownString copies a string that fiber took from the request buffer. c.Get and
// friends return strings that alias fasthttp's buffers, which the next request
// on the connection overwrites; the HTTP/2 transport keeps header strings in its
// HPACK table, and a table whose strings change under it panics the whole
// server ("id (N) <= evictCount (M)"). Header values that go upstream must be
// copied.
func OwnString(s string) string { return strings.Clone(s) }

// callPrefix is the URL prefix that carries the access token: /call/{token}/...
const CallPrefix = "/call/"

const (
	localsCfg   = "ccgp.cfg"
	LocalsModel = "ccgp.model" // "requested -> backend", shown in the request log
	LocalsErr   = "ccgp.err"   // error message of a failed request, shown in the request log
	LocalsConn  = "ccgp.conn"  // "conn new …" when the request opened a connection, shown in the request log
)

// profileBuilder returns the per-request config for a profile.
type ProfileBuilder func(profile string) (*config.Config, error)

// requireProfile resolves the :token path param to a profile and stores its
// config in Locals. Unknown token returns 404 so the endpoint is not revealed.
func RequireProfile(index *creds.TokenIndex, build ProfileBuilder) fiber.Handler {
	return func(c *fiber.Ctx) error {
		profile, ok := index.Lookup(c.Params("token"))
		if !ok {
			return c.SendStatus(fiber.StatusNotFound)
		}
		cfg, err := build(profile)
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"type":  "error",
				"error": fiber.Map{"type": "api_error", "message": err.Error()},
			})
		}
		c.Locals(localsCfg, cfg)
		return c.Next()
	}
}

// requestConfig returns the config stored by requireProfile.
func RequestConfig(c *fiber.Ctx) *config.Config {
	cfg, _ := c.Locals(localsCfg).(*config.Config)
	return cfg
}

// maskToken hides the token segment of /call/{token}/... paths for logging.
func MaskToken(path string) string {
	if !strings.HasPrefix(path, CallPrefix) {
		return path
	}
	rest := path[len(CallPrefix):]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return CallPrefix + "***" + rest[i:]
	}
	return CallPrefix + "***"
}

func AnthropicError(c *fiber.Ctx, status int, typ, msg string) error {
	c.Locals(LocalsErr, msg)
	return c.Status(status).JSON(fiber.Map{
		"type":  "error",
		"error": fiber.Map{"type": typ, "message": msg},
	})
}

// logOut receives the non-access log lines (rejected bodies, broken streams).
var LogOut io.Writer = os.Stdout

// maxLogDetail caps the raw upstream event in a stream failure line.
const maxLogDetail = 1000

// logStreamFailure prints the line for a stream that broke after its reply
// started; detail is the raw upstream event that ended it, extra the request
// ids and timing, either may be empty.
func LogStreamFailure(w io.Writer, started time.Time, what, profile, model, msg, detail, extra string) {
	line := EventText(msg, detail)
	if extra != "" {
		line += " | " + extra
	}
	_, _ = fmt.Fprintf(w, "[%s] %s failed after %s [%s] %s: %s\n", started.Format("15:04:05"), what,
		time.Since(started).Round(time.Millisecond), profile, model, line)
}

// eventText is the reason of a failure or retry line, then the raw upstream
// event without the message text the reason already holds. The reason is then
// the only copy of that text, so it is cut at maxLogDetail, not maxLogErr.
func EventText(msg, detail string) string {
	detail, dropped := DropShownMessage(detail, msg)
	n := MaxLogErr
	if dropped {
		n = maxLogDetail
	}
	line := OneLine(msg, n)
	if detail != "" {
		line += " | upstream event: " + OneLine(detail, maxLogDetail)
	}
	return line
}

// shownMessage stands in the raw event for a message the line already shows.
const ShownMessage = `"(shown before)"`

// dropShownMessage replaces each non-empty "message" string of the raw JSON
// event that msg shows (msg is it, or ends with ": " and it, as describeError
// joins a code and a message) with shownMessage; dropped tells whether any was.
// A message merely found inside msg ("a" in "bad") is another text.
func DropShownMessage(detail, msg string) (string, bool) {
	const key = `"message"`
	var b strings.Builder
	dropped := false
	rest := detail
	for {
		i := strings.Index(rest, key)
		if i < 0 {
			break
		}
		k := i + len(key)
		k += len(rest[k:]) - len(strings.TrimLeft(rest[k:], " \t\r\n"))
		if k >= len(rest) || rest[k] != ':' { // a value, not a key
			b.WriteString(rest[:k])
			rest = rest[k:]
			continue
		}
		k++
		k += len(rest[k:]) - len(strings.TrimLeft(rest[k:], " \t\r\n"))
		var s string
		dec := json.NewDecoder(strings.NewReader(rest[k:]))
		if k >= len(rest) || rest[k] != '"' || dec.Decode(&s) != nil || s == "" || msg != s && !strings.HasSuffix(msg, ": "+s) {
			b.WriteString(rest[:k])
			rest = rest[k:]
			continue
		}
		b.WriteString(rest[:k])
		b.WriteString(ShownMessage)
		rest = rest[k+int(dec.InputOffset()):]
		dropped = true
	}
	if !dropped {
		return detail, false
	}
	b.WriteString(rest)
	return b.String(), true
}

// joinNonEmpty joins the non-empty parts with " | ".
func JoinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " | ")
}

// upstreamIDs names the response's request ids, for matching a failure with
// the backend's side, and the edge's verdict (cf-mitigated: a challenge or a block).
func UpstreamIDs(h http.Header) string {
	var ids []string
	for _, k := range []string{"X-Request-Id", "Cf-Ray", "Cf-Mitigated"} {
		if v := h.Get(k); v != "" {
			ids = append(ids, strings.ToLower(k)+"="+v)
		}
	}
	return strings.Join(ids, " ")
}

// sleepCtx waits d, false if ctx ended first.
func SleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// rateRetryMax is the longest Retry-After that ccgp waits out itself before
// asking the backend once more; a longer one goes to Claude Code, which backs
// off by its own rules. A var so tests can change it.
var RateRetryMax = 5 * time.Second

// retryAfter reads a Retry-After header (seconds, or an HTTP date).
func RetryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}

// rateHeaders lists the headers that tell about limits and when they reset
// ("retry-after=2 x-ratelimit-remaining-requests=0 ..."), for the log.
func RateHeaders(h http.Header) string {
	var out []string
	for k, vs := range h {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "retry-after") || strings.Contains(lk, "ratelimit") ||
			strings.Contains(lk, "rate-limit") || strings.Contains(lk, "reset") || strings.HasPrefix(lk, "x-codex") {
			out = append(out, lk+"="+strings.Join(vs, ","))
		}
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// logRateRetry prints the line for a request that waits out a short rate limit.
func LogRateRetry(w io.Writer, profile, model string, wait time.Duration, headers string) {
	_, _ = fmt.Fprintf(w, "[%s] upstream rate limit: retrying in %s [%s] %s: %s\n", time.Now().Format("15:04:05"),
		wait.Round(time.Millisecond), profile, model, headers)
}

// maxLogErr caps the error text of a log line.
const MaxLogErr = 300

// oneLine flattens s to a single line of at most n bytes.
func OneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n], "") + "…"
	}
	return s
}
