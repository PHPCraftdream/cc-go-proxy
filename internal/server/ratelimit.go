package server

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// rateRetryMax is the longest Retry-After that ccgp waits out itself before
// asking the backend once more; a longer one goes to Claude Code, which backs
// off by its own rules. A var so tests can change it.
var rateRetryMax = 5 * time.Second

// retryAfter reads a Retry-After header (seconds, or an HTTP date).
func retryAfter(h http.Header, now time.Time) (time.Duration, bool) {
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
func rateHeaders(h http.Header) string {
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
func logRateRetry(w io.Writer, profile, model string, wait time.Duration, headers string) {
	_, _ = fmt.Fprintf(w, "[%s] upstream rate limit: retrying in %s [%s] %s: %s\n", time.Now().Format("15:04:05"),
		wait.Round(time.Millisecond), profile, model, headers)
}
