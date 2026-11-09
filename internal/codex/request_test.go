package codex

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseRequestOwnsInputAndConvertsMappedIdentity(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4","system":"You are powered by the model named Sonnet 4. The exact model ID is claude-sonnet-4.","messages":[{"role":"user","content":[{"type":"text","text":"You are powered by the model named Sonnet 4. The exact model ID is claude-sonnet-4."},{"type":"tool_result","tool_use_id":"call-1","content":"keep this result"}]}],"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],"metadata":{"user_id":"user-1"}}`)
	expectedReq, _, err := ConvertRequest(append([]byte(nil), body...), "gpt-5-mapped", "scope-a")
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(expectedReq)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := parsed.Model(), "claude-sonnet-4"; got != want {
		t.Fatalf("Model() = %q, want %q", got, want)
	}
	for i := range body {
		body[i] = 'x'
	}
	gotReq, _, err := parsed.Convert("gpt-5-mapped", "scope-a")
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(gotReq)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("conversion after source overwrite differs\\n got: %s\\nwant: %s", got, want)
	}
	if gotReq.Model != "gpt-5-mapped" {
		t.Errorf("converted model = %q, want mapped model", gotReq.Model)
	}
	wantIdentity := `You are powered by the model named gpt-5-mapped, reached through a Claude Code gateway. The exact model ID is gpt-5-mapped. Claude Code shows this session as "claude-sonnet-4"; that is only a display label, not your identity.`
	if got := gotReq.Input[0].(messageItem).Content[0].Text; got != wantIdentity {
		t.Errorf("requested-model identity = %q, want %q", got, wantIdentity)
	}
}

func TestParseBlocksDispatchesByJSONShape(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []anthropicBlock
	}{
		{"whitespace null", " \n null ", []anthropicBlock{{Type: "text"}}},
		{"null", "null", []anthropicBlock{{Type: "text"}}},
		{"whitespace string", " \n\t\"hello\" ", []anthropicBlock{{Type: "text", Text: "hello"}}},
		{"whitespace array", " \n [ {\"type\":\"text\",\"text\":\"hello\"} ] ", []anthropicBlock{{Type: "text", Text: "hello"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBlocks(json.RawMessage(tt.raw))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseBlocks() = %#v, want %#v", got, tt.want)
			}
		})
	}
	for _, raw := range []string{`{"type":"text"}`, `7`, `true`} {
		if _, err := parseBlocks(json.RawMessage(raw)); err == nil || !strings.HasPrefix(err.Error(), "content: json: cannot unmarshal") {
			t.Errorf("parseBlocks(%s) error = %v, want content unmarshal error", raw, err)
		}
	}
}

func TestSystemTextDispatchesByJSONShape(t *testing.T) {
	if got := systemText(json.RawMessage(" \n \t\"hello\" ")); got != "hello" {
		t.Errorf("systemText whitespace string = %q, want hello", got)
	}
	if got := systemText(json.RawMessage(" \n [{\"type\":\"text\",\"text\":\"hello\"}] ")); got != "hello" {
		t.Errorf("systemText whitespace array = %q, want hello", got)
	}
	for _, raw := range []string{"null", " null ", `{"type":"text"}`, "7", `"unterminated`} {
		if got := systemText(json.RawMessage(raw)); got != "" {
			t.Errorf("systemText(%s) = %q, want empty", raw, got)
		}
	}
}

func TestSystemBillingHeaderStringMatchesArray(t *testing.T) {
	for _, billing := range []string{
		"x-anthropic-billing-header: billing-a",
		"x-anthropic-billing-header: billing-b",
	} {
		t.Run(billing, func(t *testing.T) {
			remainder := "Developer prompt\n\nPreserve this.\n"
			stringForm := developerText(t, billing+"\n"+remainder)
			arrayForm := developerText(t, []map[string]string{
				{"type": "text", "text": billing},
				{"type": "text", "text": remainder},
			})
			if stringForm != remainder {
				t.Errorf("string developer text = %q, want %q", stringForm, remainder)
			}
			if stringForm != arrayForm {
				t.Errorf("string developer text = %q, array developer text = %q", stringForm, arrayForm)
			}
		})
	}
}

func TestSystemTextStringBillingHeaderEdges(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"preserves blank lines and trailing bytes", "x-anthropic-billing-header: value\n\n\nprompt\n", "\n\nprompt\n"},
		{"header without newline", "x-anthropic-billing-header: value", ""},
		{"uses array prefix semantics", "x-anthropic-billing-header-extra\nbody", "body"},
		{"does not strip a later line", "intro\nx-anthropic-billing-header: value\nbody", "intro\nx-anthropic-billing-header: value\nbody"},
		{"prefix is case sensitive", "X-anthropic-billing-header: value\nbody", "X-anthropic-billing-header: value\nbody"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := systemText(json.RawMessage(mustJSON(t, tt.in))); got != tt.want {
				t.Errorf("systemText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSystemTextOrdinaryStringUnchanged(t *testing.T) {
	ordinary := "ordinary prompt\n\nwith blank lines\n"
	if got := developerText(t, ordinary); got != ordinary {
		t.Errorf("developer text = %q, want unchanged %q", got, ordinary)
	}
}

func developerText(t *testing.T, system any) string {
	t.Helper()
	req, _, err := ConvertRequest([]byte(mustJSON(t, map[string]any{
		"model":    "claude-test",
		"system":   system,
		"messages": []any{},
	})), "gpt-test", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != 1 {
		t.Fatalf("input has %d items, want one developer message", len(req.Input))
	}
	msg, ok := req.Input[0].(messageItem)
	if !ok || msg.Role != "developer" || len(msg.Content) != 1 {
		t.Fatalf("input item = %#v, want one developer text message", req.Input[0])
	}
	return msg.Content[0].Text
}
