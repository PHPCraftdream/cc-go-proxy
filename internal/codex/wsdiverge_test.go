package codex

import (
	"encoding/json"
	"fmt"
	"testing"
)

var wsAnswer = json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`)

func TestWSDivergenceEditedLastItemKeepsTheReply(t *testing.T) {
	base := wsRequest(t, wsBody(wsUser("first")+`,{"role":"assistant","content":"a1"},`+wsUser("tail"), ""))
	chain := NewChain(base, "resp", []json.RawMessage{wsAnswer})
	next := wsRequest(t, wsBody(wsUser("first")+`,{"role":"assistant","content":"a1"},`+wsUser("tail plus more")+
		`,{"role":"assistant","content":"answer"},`+wsUser("again"), ""))
	n := len(base.Input)

	_, ok, reason, d := chain.delta(next)
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
	_, ok, _, d := chain.delta(next)
	if ok || d == nil || d.At != len(base.Input)-1 || d.SameHead < 0 || d.ReplyAt != -1 || !d.Reply {
		t.Fatalf("delta ok=%v div=%+v", ok, d)
	}
}

func TestWSDivergenceShorterAndEarlyChanges(t *testing.T) {
	base := wsRequest(t, wsBody(wsUser("first")+`,{"role":"assistant","content":"a1"},`+wsUser("tail"), ""))
	chain := NewChain(base, "resp", nil)

	shorter := *base
	shorter.Input = base.Input[:len(base.Input)-1]
	_, ok, reason, d := chain.delta(&shorter)
	if ok || d == nil || d.At != len(shorter.Input) || d.Now.Kind != "" || d.SameHead != -1 || d.Reply {
		t.Fatalf("shorter: ok=%v reason=%q div=%+v", ok, reason, d)
	}

	early := wsRequest(t, wsBody(wsUser("changed")+`,{"role":"assistant","content":"a1"},`+wsUser("tail"), ""))
	_, ok, _, d = chain.delta(early)
	if ok || d == nil || d.At != 0 || d.SameHead != -1 || d.SameTail != -1 || d.Was.Kind != "user message" {
		t.Fatalf("early: ok=%v div=%+v", ok, d)
	}

	if _, _, _, d := chain.delta(nil); d != nil {
		t.Errorf("no request gave a divergence %+v", d)
	}
}

func TestItemShapeNamesKindsOnly(t *testing.T) {
	cases := []struct {
		item any
		want ItemShape
	}{
		{messageItem{Type: "message", Role: "assistant", Content: []contentPart{{}, {}}}, ItemShape{Kind: "assistant message", Parts: 2}},
		{functionCallItem{Type: "function_call"}, ItemShape{Kind: "function_call"}},
		{functionOutputItem{Type: "function_call_output"}, ItemShape{Kind: "function_call_output"}},
		{reasoningItem{Type: "reasoning"}, ItemShape{Kind: "reasoning"}},
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
