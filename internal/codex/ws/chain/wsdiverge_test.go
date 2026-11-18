package chain

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/catalog"
)

var wsAnswer = json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`)

func TestWSDivergenceEditedLastItemKeepsTheReply(t *testing.T) {
	base := wsRequest(t, wsBody(wsUser("first")+`,{"role":"assistant","content":"a1"},`+wsUser("tail"), ""))
	chain := NewChain(base, "resp", []json.RawMessage{wsAnswer})
	next := wsRequest(t, wsBody(wsUser("first")+`,{"role":"assistant","content":"a1"},`+wsUser("tail plus more")+
		`,{"role":"assistant","content":"answer"},`+wsUser("again"), ""))
	n := len(base.Input)

	_, ok, reason, d := chain.DeltaWith(next, nil)
	if ok || reason != fmt.Sprintf("history diverged at item %d of %d", n-1, n) || d == nil {
		t.Fatalf("delta ok=%v reason=%q div=%+v", ok, reason, d)
	}
	if d.At != n-1 || d.Had != n || d.Has != len(next.Input) {
		t.Errorf("position = %+v", d)
	}
	if d.Was.Kind != "user message" || d.Now.Kind != "user message" || d.Was.Parts != 1 || d.Now.Parts != 1 {
		t.Errorf("shapes = %+v / %+v", d.Was, d.Now)
	}
	if grown := d.Now.Size - d.Was.Size; grown != len(" plus more") {
		t.Errorf("size grew by %d, want %d", grown, len(" plus more"))
	}
	if d.SameHead+d.SameTail != d.Was.Size || d.Now.Size-d.SameHead-d.SameTail != len(" plus more") {
		t.Errorf("equal ends %d/%d of %d -> %d", d.SameHead, d.SameTail, d.Was.Size, d.Now.Size)
	}
	if !d.Reply || d.ReplyAt != n {
		t.Errorf("reply at %d (reply %v), want %d", d.ReplyAt, d.Reply, n)
	}
}

func TestWSDivergenceReplacedLastItemHasNoReply(t *testing.T) {
	base := wsRequest(t, wsBody(wsUser("first")+`,{"role":"assistant","content":"a1"},`+wsUser("side question"), ""))
	chain := NewChain(base, "resp", []json.RawMessage{wsAnswer})
	next := wsRequest(t, wsBody(wsUser("first")+`,{"role":"assistant","content":"a1"},`+wsUser("main question"), ""))
	_, ok, _, d := chain.DeltaWith(next, nil)
	if ok || d == nil || d.At != len(base.Input)-1 || d.SameHead < 0 || d.ReplyAt != -1 || !d.Reply {
		t.Fatalf("delta ok=%v div=%+v", ok, d)
	}
}

func TestWSDivergenceShorterAndEarlyChanges(t *testing.T) {
	base := wsRequest(t, wsBody(wsUser("first")+`,{"role":"assistant","content":"a1"},`+wsUser("tail"), ""))
	chain := NewChain(base, "resp", nil)

	shorter := *base
	shorter.Input = base.Input[:len(base.Input)-1]
	_, ok, reason, d := chain.DeltaWith(&shorter, nil)
	if ok || d == nil || d.At != len(shorter.Input) || d.Now.Kind != "" || d.SameHead != -1 || d.Reply {
		t.Fatalf("shorter: ok=%v reason=%q div=%+v", ok, reason, d)
	}

	early := wsRequest(t, wsBody(wsUser("changed")+`,{"role":"assistant","content":"a1"},`+wsUser("tail"), ""))
	_, ok, _, d = chain.DeltaWith(early, nil)
	if ok || d == nil || d.At != 0 || d.SameHead != -1 || d.SameTail != -1 || d.Was.Kind != "user message" {
		t.Fatalf("early: ok=%v div=%+v", ok, d)
	}

	if _, _, _, d := chain.DeltaWith(nil, nil); d != nil {
		t.Errorf("no request gave a divergence %+v", d)
	}
}

func TestItemShapeNamesKindsOnly(t *testing.T) {
	cases := []struct {
		item any
		want ItemShape
	}{
		{codex.MessageItem{Type: "message", Role: "assistant", Content: []codex.ContentPart{{}, {}}}, ItemShape{Kind: "assistant message", Parts: 2}},
		{codex.FunctionCallItem{Type: "function_call"}, ItemShape{Kind: "function_call"}},
		{codex.FunctionOutputItem{Type: "function_call_output"}, ItemShape{Kind: "function_call_output"}},
		{codex.ReasoningItem{Type: "reasoning"}, ItemShape{Kind: "reasoning"}},
		{map[string]any{"type": "message", "role": "user", "content": []any{1}}, ItemShape{Kind: "user message", Parts: 1}},
		{map[string]any{"type": "message", "role": "secret role"}, ItemShape{Kind: "message"}},
		{map[string]any{"type": "web_search_call"}, ItemShape{Kind: "web_search_call"}},
		{map[string]any{"type": "Secret Text"}, ItemShape{Kind: "other"}},
		{"plain", ItemShape{Kind: "other"}},
	}
	for _, tc := range cases {
		if got := itemShape(tc.item); got != tc.want {
			t.Errorf("itemShape(%#v) = %+v, want %+v", tc.item, got, tc.want)
		}
	}
}

func TestCommonEndsNeverOverlap(t *testing.T) {
	cases := []struct {
		a, b       string
		head, tail int
	}{
		{"abc", "abc", 3, 0},
		{"aa", "aaa", 2, 0},
		{"abXcd", "abYYcd", 2, 2},
		{"xyz", "abc", 0, 0},
		{"", "abc", 0, 0},
	}
	for _, tc := range cases {
		head, tail := commonEnds([]byte(tc.a), []byte(tc.b))
		if head != tc.head || tail != tc.tail {
			t.Errorf("commonEnds(%q, %q) = %d, %d; want %d, %d", tc.a, tc.b, head, tail, tc.head, tc.tail)
		}
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
		_, ok, reason := chain.Delta(&codex.Request{PromptCacheKey: "key", ServiceTier: tc.next})
		if ok || reason != tc.reason {
			t.Errorf("chain %q next %q: ok=%v reason=%q, want %q", tc.chain, tc.next, ok, reason, tc.reason)
		}
	}
}

func TestFastWireCatalogAndSelection(t *testing.T) {
	models, err := catalog.ParseCatalog([]byte(`{"models":[{"slug":"example","service_tiers":[{"id":"custom-priority","name":"Fast"}],"additional_speed_tiers":["fast"]},{"slug":"no-tier","additional_speed_tiers":["fast"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	id, err := catalog.FastServiceTier(models, "example")
	if err != nil || id != "custom-priority" {
		t.Fatalf("tier = %q, %v", id, err)
	}
	for _, slug := range []string{"missing", "no-tier"} {
		if _, err := catalog.FastServiceTier(models, slug); err == nil {
			t.Fatalf("%s accepted", slug)
		}
	}
	for _, tc := range []struct {
		speed string
		fast  bool
	}{{`"fast"`, true}, {`"standard"`, false}, {`null`, false}, {`"other"`, false}} {
		p, err := codex.ParseRequest([]byte(`{"model":"x","speed":` + tc.speed + `}`))
		if err != nil || p.ClientFast() != tc.fast {
			t.Fatalf("speed %s: %v", tc.speed, err)
		}
	}
	p, err := codex.ParseRequest([]byte(`{"model":"x"}`))
	if err != nil || p.ClientFast() {
		t.Fatal("absent speed selected fast")
	}
	req := &codex.Request{Model: "example", ServiceTier: id}
	for _, ws := range []bool{false, true} {
		var body []byte
		if ws {
			body, err = WSCreatePayload(req, "previous", nil)
		} else {
			body, err = json.Marshal(req)
		}
		if err != nil || !strings.Contains(string(body), `"service_tier":"custom-priority"`) {
			t.Fatalf("wire %s: %v", body, err)
		}
	}
	req.ServiceTier = ""
	body, err := json.Marshal(req)
	if err != nil || strings.Contains(string(body), "service_tier") {
		t.Fatalf("standard wire %s: %v", body, err)
	}
}

func TestFastTierChangeForcesFullWebsocket(t *testing.T) {
	chain := &WSChain{responseID: "id", serviceTier: "priority"}
	_, ok, reason := chain.Delta(&codex.Request{PromptCacheKey: "key"})
	if ok || reason != "service tier changed" {
		t.Fatalf("delta %v: %s", ok, reason)
	}
}

func m(slug string, window int64, hidden bool) catalog.Model {
	return catalog.Model{Slug: slug, ContextWindow: window, Hidden: hidden}
}
