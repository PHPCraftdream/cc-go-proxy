package codex

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFastServiceTierCatalogShapes(t *testing.T) {
	models, err := ParseCatalog([]byte(`{"models":[
		{"slug":"spaced","service_tiers":[{"id":"p1","name":"  fast "}]},
		{"slug":"blank-id-first","service_tiers":[{"id":"","name":"Fast"},{"id":"p2","name":"Fast"}]},
		{"slug":"null-tiers","service_tiers":null},
		{"slug":"no-field"},
		{"slug":"broken","service_tiers":"oops"},
		{"slug":"standard-only","service_tiers":[{"id":"s","name":"Standard"}]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	for slug, want := range map[string]string{"spaced": "p1", "blank-id-first": "p2"} {
		if got, err := FastServiceTier(models, slug); err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", slug, got, err, want)
		}
	}
	for slug, msg := range map[string]string{
		"null-tiers":    "does not advertise",
		"no-field":      "does not advertise",
		"standard-only": "does not advertise",
		"broken":        "invalid service tiers",
		"absent":        "absent from the account catalog",
	} {
		if got, err := FastServiceTier(models, slug); err == nil || !strings.Contains(err.Error(), msg) || got != "" {
			t.Errorf("%s: %q, %v; want an error with %q", slug, got, err, msg)
		}
	}
}

// Only the top-level string "fast" selects Fast; anything else parses and is not Fast.
func TestClientFastOnlyForTheFastString(t *testing.T) {
	for _, speed := range []string{`1`, `true`, `{"a":1}`, `["fast"]`, `"FAST"`, `"fast "`, `""`} {
		p, err := ParseRequest([]byte(`{"model":"x","speed":` + speed + `}`))
		if err != nil {
			t.Fatalf("speed %s rejected: %v", speed, err)
		}
		if p.ClientFast() {
			t.Errorf("speed %s selected Fast", speed)
		}
	}
	p, err := ParseRequest([]byte(`{"model":"x","metadata":{"speed":"fast"}}`))
	if err != nil || p.ClientFast() {
		t.Errorf("nested speed selected Fast: %v", err)
	}
}

// The client's speed field is not forwarded: only a resolved tier reaches the wire.
func TestSpeedFieldIsNotForwarded(t *testing.T) {
	p, err := ParseRequest([]byte(`{"model":"x","speed":"fast","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req, _, err := p.Convert("gpt-6.2-sol", "codex")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if req.ServiceTier != "" || strings.Contains(string(body), "speed") || strings.Contains(string(body), "service_tier") {
		t.Errorf("request body: %s", body)
	}
}

// A chain of the same tier is judged on its other merits; a changed tier, in
// either direction, never continues it.
func TestChainDeltaTierComparison(t *testing.T) {
	for _, tc := range []struct{ chain, next, reason string }{
		{"priority", "priority", "no response id"},
		{"", "", "no response id"},
		{"", "priority", "service tier changed"},
		{"priority", "", "service tier changed"},
		{"priority", "other", "service tier changed"},
	} {
		chain := &WSChain{serviceTier: tc.chain}
		_, ok, reason := chain.Delta(&Request{PromptCacheKey: "key", ServiceTier: tc.next})
		if ok || reason != tc.reason {
			t.Errorf("chain %q next %q: ok=%v reason=%q, want %q", tc.chain, tc.next, ok, reason, tc.reason)
		}
	}
}
