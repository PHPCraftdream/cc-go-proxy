package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

// catalogClientVersion selects the model catalog revision (Codex CLI version).
const catalogClientVersion = "0.159.0"

// Model is one entry of the account's Codex model catalog.
type Model struct {
	Slug          string
	Name          string
	ContextWindow int64 // default window of Codex clients; 0 when the catalog does not say
	// MaxContextWindow is the largest input the backend accepts for the model
	// (the catalog's max_context_window); 0 when absent.
	MaxContextWindow int64
	// Hidden entries are not offered in pickers but may still be usable; they
	// qualify only as the previous release (Haiku), never as the newest.
	Hidden bool
	// Raw holds the catalog entry as received (for `ccgp models -v`).
	Raw map[string]json.RawMessage
}

// FetchCatalog reads the models available to the signed-in account.
// baseURL is the Codex backend root (DefaultBaseURL when empty).
func FetchCatalog(ctx context.Context, baseURL string, tok *oauth.Tokens) ([]Model, error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	root := strings.TrimSuffix(baseURL, "/codex")

	var lastErr error
	for _, u := range []string{baseURL + "/models", root + "/codex/models", root + "/models"} {
		models, status, err := fetchCatalog(ctx, u, tok)
		switch {
		case err == nil:
			return models, nil
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			return nil, fmt.Errorf("codex model catalog rejected the account (HTTP %d): does this ChatGPT account have Codex access?", status)
		}
		lastErr = err
	}
	return nil, lastErr
}

func fetchCatalog(ctx context.Context, rawURL string, tok *oauth.Tokens) ([]Model, int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, 0, err
	}
	q := u.Query()
	q.Set("client_version", catalogClientVersion)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	if tok.AccountID != "" {
		req.Header.Set("ChatGPT-Account-ID", tok.AccountID)
	}
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("originator", originator)
	req.Header.Set("version", catalogClientVersion)
	req.Header.Set("Accept", "application/json")

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, resp.StatusCode, fmt.Errorf("codex model catalog: HTTP %d", resp.StatusCode)
	}
	models, err := ParseCatalog(body)
	return models, resp.StatusCode, err
}

// ParseCatalog decodes {"models":[...]} or {"data":[...]}.
func ParseCatalog(body []byte) ([]Model, error) {
	var payload struct {
		Models *[]map[string]json.RawMessage `json:"models"`
		Data   *[]map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode model catalog: %w", err)
	}
	entries := payload.Models
	if entries == nil {
		entries = payload.Data
	}
	if entries == nil {
		return nil, errors.New("model catalog has no models or data array")
	}
	var out []Model
	for _, f := range *entries {
		slug := str(f["slug"])
		if slug == "" {
			slug = str(f["id"])
		}
		if slug == "" {
			continue
		}
		v := strings.ToLower(str(f["visibility"]))
		name := str(f["display_name"])
		if name == "" {
			name = slug
		}
		out = append(out, Model{Slug: slug, Name: name, ContextWindow: num(f["context_window"]), MaxContextWindow: num(f["max_context_window"]), Hidden: v == "hide" || v == "hidden", Raw: f})
	}
	return out, nil
}

func str(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

func num(raw json.RawMessage) int64 {
	var n json.Number
	if len(raw) == 0 || json.Unmarshal(raw, &n) != nil {
		return 0
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil || f <= 0 || f > 1<<53 {
		return 0
	}
	return int64(f)
}

// Selection maps Claude tiers to Codex model slugs.
type Selection struct {
	Fable, Opus, Sonnet, Haiku string
	// Per-model context windows in tokens. A model without a window in the
	// catalog gets DefaultContextWindow (and a note), so every value is explicit.
	FableWindow, OpusWindow, SonnetWindow, HaikuWindow int64
	Notes                                              []string
}

// DefaultContextWindow is assumed for models whose catalog entry has none
// (the Codex catalog default).
const DefaultContextWindow = 272_000

var gpt6Re = regexp.MustCompile(`^gpt-(\d+(?:\.\d+)*)-(astra|sol|luna)$`)

type release struct {
	slug    string
	version []int
	window  int64 // context_window
	max     int64 // max_context_window
	hidden  bool
}

// Pick chooses the latest models per tier from the catalog:
//
//	Fable  -> newest astra
//	Opus   -> newest sol
//	Sonnet -> newest luna
//	Haiku  -> luna of the release before the newest luna
func Pick(models []Model) (Selection, error) {
	tiers := map[string][]release{}
	for _, m := range models {
		g := gpt6Re.FindStringSubmatch(m.Slug)
		if g == nil {
			continue
		}
		v, ok := parseVersion(g[1])
		if !ok {
			continue
		}
		tiers[g[2]] = append(tiers[g[2]], release{m.Slug, v, m.ContextWindow, m.MaxContextWindow, m.Hidden})
	}
	for _, t := range tiers {
		sort.Slice(t, func(i, j int) bool { return cmpVersion(t[i].version, t[j].version) > 0 })
	}

	var sel Selection
	var missing []string
	// newest: the highest non-hidden release of a tier.
	newest := func(tier string) (release, bool) {
		for _, r := range tiers[tier] {
			if !r.hidden {
				return r, true
			}
		}
		return release{}, false
	}
	// before: the highest release of a tier older than ref; hidden ones count.
	before := func(tier string, ref release) (release, bool) {
		for _, r := range tiers[tier] {
			if cmpVersion(r.version, ref.version) < 0 {
				return r, true
			}
		}
		return release{}, false
	}
	// windowOf is the input size the model really takes: the GPT-6 generation
	// (and newer) runs up to the catalog's max_context_window, while the catalog's
	// context_window is only a default of Codex clients; earlier releases are
	// held to that default.
	windowOf := func(r release) int64 {
		if r.version[0] >= 6 && r.max > 0 {
			return r.max
		}
		if r.window > 0 {
			return r.window
		}
		sel.Notes = append(sel.Notes, fmt.Sprintf("catalog gives no context window for %s: assuming %d", r.slug, DefaultContextWindow))
		return DefaultContextWindow
	}
	if r, ok := newest("astra"); ok {
		sel.Fable, sel.FableWindow = r.slug, windowOf(r)
	} else {
		missing = append(missing, "astra (Fable)")
	}
	if r, ok := newest("sol"); ok {
		sel.Opus, sel.OpusWindow = r.slug, windowOf(r)
	} else {
		missing = append(missing, "sol (Opus)")
	}
	if r, ok := newest("luna"); ok {
		sel.Sonnet, sel.SonnetWindow = r.slug, windowOf(r)
		prev, hasPrev := before("luna", r)
		if !hasPrev {
			prev = r
			sel.Notes = append(sel.Notes, "only one luna release is available: Haiku uses the same model as Sonnet")
		}
		sel.Haiku, sel.HaikuWindow = prev.slug, windowOf(prev)
	} else {
		missing = append(missing, "luna (Sonnet, Haiku)")
	}
	if len(missing) > 0 {
		return sel, fmt.Errorf("catalog has no %s model", strings.Join(missing, ", "))
	}
	return sel, nil
}

func parseVersion(s string) ([]int, bool) {
	parts := strings.Split(s, ".")
	v := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		v[i] = n
	}
	return v, true
}

func cmpVersion(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}
