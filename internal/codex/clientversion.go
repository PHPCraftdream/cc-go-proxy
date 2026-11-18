package codex

import (
	"net/http"
	"regexp"
)

var (
	// What the backend says when a model needs a newer client than the one
	// presented ("The 'X' model requires a newer version of Codex. Please upgrade
	// to the latest app or CLI and try again."). A plan upsell ("upgrade to Pro")
	// is not one of them.
	newerClientRe  = regexp.MustCompile(`(?i)(requires? a (newer|more recent) (version|release)|newer version of (the )?codex|upgrade to the latest (codex|app|cli|version)|(update|upgrade) (your )?codex|client version (is )?(too old|outdated|not supported|unsupported)|(unsupported|outdated) client version|minimum (client )?version)`)
	versionFieldRe = regexp.MustCompile(`"(?:minimal|minimum|min|required)_(?:client_)?version"\s*:\s*(?:"(\d+\.\d+\.\d+)"|\[\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)\s*\])`)
	versionTextRe  = regexp.MustCompile(`\b(\d+\.\d+\.\d+)\b`)
)

// NeedsNewerClient reports whether an upstream error says the presented client
// version is too old, and the version it asks for when the error names one.
func NeedsNewerClient(status int, body []byte) (required string, ok bool) {
	if status < 400 || status >= 500 || status == http.StatusUnauthorized || status == http.StatusTooManyRequests || IsUsageLimit(body) {
		return "", false
	}
	msg := ErrorMessage(body)
	if !newerClientRe.MatchString(msg) && !versionFieldRe.Match(body) {
		return "", false
	}
	if m := versionFieldRe.FindSubmatch(body); m != nil {
		if len(m[1]) > 0 {
			return string(m[1]), true
		}
		return string(m[2]) + "." + string(m[3]) + "." + string(m[4]), true
	}
	if m := versionTextRe.FindStringSubmatch(msg); m != nil {
		return m[1], true
	}
	return "", true
}
