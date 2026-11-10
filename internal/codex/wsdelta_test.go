package codex

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func wsRequest(t *testing.T, body string) *Request {
	t.Helper()
	parsed, err := ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	req, _, err := parsed.Convert("gpt-5", "ws-test")
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func wsBody(messages string, controls string) string {
	return `{"model":"claude-test","metadata":{"user_id":"session"},` + controls + `"messages":[` + messages + `]}`
}

func wsUser(text string) string {
	b, _ := json.Marshal(text)
	return `{"role":"user","content":` + string(b) + `}`
}

func TestWSChainToolLoopParallelCallsAndOutputs(t *testing.T) {
	base := wsRequest(t, wsBody(wsUser("find"), `"tools":[{"name":"one","input_schema":{"type":"object"}},{"name":"two","input_schema":{"type":"object"}}],`))
	output := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","call_id":"c1","name":"one","arguments":"{\"x\":1}"}`),
		json.RawMessage(`{"type":"function_call","call_id":"c2","name":"two","arguments":"{}"}`),
	}
	chain := NewChain(base, "resp-tools", output)
	assistant := `{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"one","input":{"x":1}},{"type":"tool_use","id":"c2","name":"two","input":{}}]}`
	results := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"one result"},{"type":"tool_result","tool_use_id":"c2","content":"two result"}]}`
	next := wsRequest(t, wsBody(wsUser("find")+`,`+assistant+`,`+results+`,`+wsUser("next"), `"tools":[{"name":"one","input_schema":{"type":"object"}},{"name":"two","input_schema":{"type":"object"}}],`))
	got, ok, reason := chain.Delta(next)
	if !ok || reason != "" || len(got) != 3 {
		t.Fatalf("Delta() = %#v, %v, %q; want three tool outputs/new item", got, ok, reason)
	}
	if inputKind(got[0]) != "function_call_output" || inputKind(got[1]) != "function_call_output" || inputKind(got[2]) != "message" {
		t.Fatalf("delta order = %#v", got)
	}
}

func TestWSChainTextAnswerThenUserAndNothingNew(t *testing.T) {
	base := wsRequest(t, wsBody(wsUser("question"), ""))
	answer := json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`)
	chain := NewChain(base, "resp-answer", []json.RawMessage{answer})
	completed := wsRequest(t, wsBody(wsUser("question")+`,{"role":"assistant","content":"answer"}`, ""))
	if got, ok, reason := chain.Delta(completed); ok || reason != "nothing new" || len(got) != 0 {
		t.Fatalf("completed Delta() = %#v, %v, %q; want nothing new", got, ok, reason)
	}
	next := wsRequest(t, wsBody(wsUser("question")+`,{"role":"assistant","content":"answer"},`+wsUser("next"), ""))
	got, ok, reason := chain.Delta(next)
	if !ok || reason != "" || len(got) != 1 || inputKind(got[0]) != "message" {
		t.Fatalf("next Delta() = %#v, %v, %q", got, ok, reason)
	}
}

func TestWSChainPrefixCompactionAndReasoningConversion(t *testing.T) {
	base := wsRequest(t, wsBody(wsUser("earlier")+`,`+wsUser("question"), ""))
	output := []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"before"}]}`),
		json.RawMessage(`{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}],"encrypted_content":"secret"}`),
		json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"after"}]}`),
	}
	chain := NewChain(base, "resp-reason", output)
	ourThinking := `{"type":"thinking","thinking":"think","signature":"ccgp1:secret"}`
	foreignThinking := `{"type":"thinking","thinking":"untrusted","signature":"foreign:sig"}`
	assistant := `{"role":"assistant","content":[{"type":"text","text":"before"},` + ourThinking + `,{"type":"text","text":"after"},` + foreignThinking + `]}`
	next := wsRequest(t, wsBody(wsUser("earlier")+`,`+wsUser("question")+`,`+assistant+`,`+wsUser("next"), ""))
	got, ok, reason := chain.Delta(next)
	if !ok || reason != "" || len(got) != 1 {
		t.Fatalf("Delta() = %#v, %v, %q", got, ok, reason)
	}
	if inputKind(got[0]) != "message" {
		t.Fatalf("new input = %#v, want next user message", got[0])
	}

	foreignOnly := `{"role":"assistant","content":[{"type":"thinking","thinking":"unsigned","signature":""},{"type":"thinking","thinking":"prefix only","signature":"ccgp1:"},{"type":"text","text":"after"}]}`
	withoutConvertedReasoning := wsRequest(t, wsBody(wsUser("earlier")+`,`+wsUser("question")+`,{"role":"assistant","content":"before"},`+foreignOnly+`,`+wsUser("next"), ""))
	// The backend holds reasoning the client no longer sends: send in full.
	got, ok, reason = chain.Delta(withoutConvertedReasoning)
	if ok || reason != "reasoning dropped" {
		t.Fatalf("unsigned/prefix-only reasoning Delta() = %#v, %v, %q; want reasoning dropped", got, ok, reason)
	}

	droppedReasoningChain := NewChain(base, "resp-dropped-reasoning", []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","summary":[],"encrypted_content":"secret"}`),
		json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"after"}]}`),
	})
	droppedThinking := `{"role":"assistant","content":[{"type":"thinking","thinking":"not retained","signature":"foreign:sig"},{"type":"text","text":"after"}]}`
	dropped := wsRequest(t, wsBody(wsUser("earlier")+`,`+wsUser("question")+`,`+droppedThinking+`,`+wsUser("next"), ""))
	if _, ok, reason := droppedReasoningChain.Delta(dropped); ok || reason != "reasoning dropped" {
		t.Fatalf("dropped reasoning Delta() = %v, %q; want reasoning dropped", ok, reason)
	}

	compacted := wsRequest(t, wsBody(wsUser("question"), ""))
	if _, ok, reason := chain.Delta(compacted); ok || reason != "history diverged at item 0" {
		t.Fatalf("compacted divergent history = %v, %q", ok, reason)
	}
}

func TestWSChainBranchesControlsAndOutputMismatch(t *testing.T) {
	baseBody := wsBody(wsUser("base"), `"system":"system","tools":[{"name":"f","input_schema":{"type":"object"}}],`)
	base := wsRequest(t, baseBody)
	chain := NewChain(base, "resp", []json.RawMessage{json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`)})
	branch := wsRequest(t, wsBody(wsUser("branch"), `"system":"system","tools":[{"name":"f","input_schema":{"type":"object"}}],`))
	if _, ok, reason := chain.Delta(branch); ok || reason != "history diverged at item 1" {
		t.Fatalf("same-key branch = %v, %q; want history divergence", ok, reason)
	}

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"model", `{"model":"claude-test","metadata":{"user_id":"session"},"system":"system","tools":[{"name":"f","input_schema":{"type":"object"}}],"messages":[` + wsUser("base") + `]}`, ""},
		{"system", wsBody(wsUser("base"), `"system":"changed","tools":[{"name":"f","input_schema":{"type":"object"}}],`), "system changed"},
		{"reasoning", wsBody(wsUser("base"), `"system":"system","tools":[{"name":"f","input_schema":{"type":"object"}}],"thinking":{"type":"enabled","budget_tokens":20000},`), "reasoning changed"},
		{"tool choice", wsBody(wsUser("base"), `"system":"system","tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":{"type":"none"},`), "tool choice changed"},
		{"tools", wsBody(wsUser("base"), `"system":"system","tools":[{"name":"f","input_schema":{"type":"object"}},{"name":"g","input_schema":{"type":"object"}}],`), "tools changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := wsRequest(t, tc.body)
			if tc.name == "model" {
				next.Model = "different"
				tc.want = "model changed"
			}
			if _, ok, reason := chain.Delta(next); ok || reason != tc.want {
				t.Fatalf("Delta() = %v, %q; want %q", ok, reason, tc.want)
			}
		})
	}
	mismatch := NewChain(base, "resp", []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"part one"}]}`),
		json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"part two"}]}`),
	})
	next := wsRequest(t, wsBody(wsUser("base")+`,{"role":"assistant","content":"answer"},`+wsUser("next"), `"system":"system","tools":[{"name":"f","input_schema":{"type":"object"}}],`))
	if _, ok, reason := mismatch.Delta(next); ok || reason != "assistant text changed" {
		t.Fatalf("output count mismatch = %v, %q", ok, reason)
	}
}

func TestWSChainOutputSequenceCountAndMalformed(t *testing.T) {
	base := wsRequest(t, wsBody(wsUser("base"), ""))
	assistantMessage := func(text string) messageItem {
		return messageItem{Type: "message", Role: "assistant", Content: []contentPart{{Type: "output_text", Text: text}}}
	}
	userNext := messageItem{Type: "message", Role: "user", Content: []contentPart{{Type: "input_text", Text: "next"}}}
	call := functionCallItem{Type: "function_call", CallID: "call-1", Name: "lookup", Arguments: `{ "x" : 1 }`}
	reasoning := reasoningItem{Type: "reasoning", EncryptedContent: "secret", Summary: []summaryPart{}}
	messageOutput := json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"after"}]}`)
	reasoningOutput := json.RawMessage(`{"type":"reasoning","summary":[],"encrypted_content":"secret"}`)
	callOutput := json.RawMessage(`{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{\"x\":1}"}`)
	call2 := func(name, args string) functionCallItem {
		return functionCallItem{Type: "function_call", CallID: "call-1", Name: name, Arguments: args}
	}
	const mismatch = "output mismatch"

	cases := []struct {
		name   string
		output []json.RawMessage
		items  []any
		reason string // "" = the delta is allowed
	}{
		{"same message text matches", []json.RawMessage{messageOutput}, []any{assistantMessage("after"), userNext}, ""},
		{"message text must match", []json.RawMessage{messageOutput}, []any{assistantMessage("changed text"), userNext}, "assistant text changed"},
		{"two output messages are not one input message", []json.RawMessage{
			json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"part one"}]}`),
			json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"part two"}]}`),
		}, []any{assistantMessage("merged"), userNext}, "assistant text changed"},
		{"message must be assistant role", []json.RawMessage{messageOutput}, []any{messageItem{Type: "message", Role: "user"}, userNext}, mismatch},
		{"reasoning cannot be reordered ahead of message", []json.RawMessage{messageOutput, reasoningOutput, messageOutput}, []any{reasoning, assistantMessage("one"), assistantMessage("two"), userNext}, mismatch},
		{"kept reasoning matches", []json.RawMessage{reasoningOutput, messageOutput}, []any{reasoning, assistantMessage("after"), userNext}, ""},
		{"reasoning must not be dropped", []json.RawMessage{reasoningOutput, messageOutput}, []any{assistantMessage("after"), userNext}, "reasoning dropped"},
		{"present reasoning encrypted content must match", []json.RawMessage{reasoningOutput}, []any{reasoningItem{Type: "reasoning", EncryptedContent: "different"}, userNext}, "reasoning differs"},
		{"reasoning cannot skip input reasoning before message", []json.RawMessage{messageOutput}, []any{reasoning, assistantMessage("after"), userNext}, mismatch},
		{"function arguments whitespace and key order may vary", []json.RawMessage{json.RawMessage(`{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{\"y\":2,\"x\":1}"}`)}, []any{call2("lookup", `{ "x" : 1, "y": 2 }`), userNext}, ""},
		{"function arguments must match", []json.RawMessage{callOutput}, []any{call2("lookup", `{"x":2}`), userNext}, "tool call changed"},
		{"function name must match", []json.RawMessage{callOutput}, []any{call2("other", `{"x":1}`), userNext}, "tool call changed"},
		{"empty arguments equal an empty object", []json.RawMessage{json.RawMessage(`{"type":"function_call","call_id":"call-1","name":"lookup","arguments":""}`)}, []any{call2("lookup", `{}`), userNext}, ""},
		{"function call cannot reorder input reasoning", []json.RawMessage{callOutput, reasoningOutput}, []any{reasoning, call, userNext}, mismatch},
		{"message and call type order must match", []json.RawMessage{messageOutput, callOutput}, []any{call, assistantMessage("after"), userNext}, mismatch},
		{"output the client does not send back", []json.RawMessage{messageOutput, callOutput}, []any{assistantMessage("after")}, "output missing"},
		{"function call output is unsupported", []json.RawMessage{json.RawMessage(`{"type":"function_call_output","call_id":"call-1","output":"result"}`)}, []any{userNext}, mismatch},
		{"malformed output is rejected", []json.RawMessage{json.RawMessage(`{"type":"message"`)}, []any{userNext}, mismatch},
		{"unknown output type is rejected", []json.RawMessage{json.RawMessage(`{"type":"image"}`)}, []any{userNext}, mismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chain := NewChain(base, "resp", tc.output)
			next := *base
			next.Input = append(append([]any(nil), base.Input...), tc.items...)
			_, ok, reason := chain.Delta(&next)
			if want := tc.reason == ""; ok != want || reason != tc.reason {
				t.Fatalf("Delta() ok=%v reason=%q, want reason %q", ok, reason, tc.reason)
			}
		})
	}
}

func TestWSChainStartsSecondCycleFromFullInput(t *testing.T) {
	first := wsRequest(t, wsBody(wsUser("first question"), ""))
	firstAnswer := messageItem{Type: "message", Role: "assistant", Content: []contentPart{{Type: "output_text", Text: "first answer"}}}
	secondInput := *first
	secondInput.Input = append(append([]any(nil), first.Input...), firstAnswer,
		messageItem{Type: "message", Role: "user", Content: []contentPart{{Type: "input_text", Text: "second question"}}})
	secondOutput := json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second answer"}]}`)
	chain := NewChain(&secondInput, "resp-second", []json.RawMessage{secondOutput})
	if chain == nil || chain.inputLen != len(secondInput.Input) {
		t.Fatalf("second cycle chain input = %#v, want full %d-item input", chain, len(secondInput.Input))
	}
	next := secondInput
	next.Input = append(append([]any(nil), secondInput.Input...),
		messageItem{Type: "message", Role: "assistant", Content: []contentPart{{Type: "output_text", Text: "second answer"}}},
		messageItem{Type: "message", Role: "user", Content: []contentPart{{Type: "input_text", Text: "third question"}}})
	got, ok, reason := chain.Delta(&next)
	if !ok || reason != "" || len(got) != 1 || inputKind(got[0]) != "message" {
		t.Fatalf("second cycle Delta() = %#v, %v, %q", got, ok, reason)
	}
}

func TestWSCreatePayloadOmitsOptionalFieldsAndForcesStream(t *testing.T) {
	req := &Request{Model: "gpt-5", Stream: false, Input: []any{messageItem{Type: "message", Role: "user"}}}
	got, err := WSCreatePayload(req, "", req.Input)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	if string(payload["stream"]) != "true" {
		t.Fatalf("stream = %s, want true", payload["stream"])
	}
	for _, key := range []string{"tools", "reasoning", "prompt_cache_key", "previous_response_id"} {
		if _, exists := payload[key]; exists {
			t.Errorf("optional field %q was serialized: %s", key, got)
		}
	}
}

func TestWSChainRejectsMismatchedFunctionCallID(t *testing.T) {
	controls := `"tools":[{"name":"lookup","input_schema":{"type":"object"}}],`
	base := wsRequest(t, wsBody(wsUser("call"), controls))
	chain := NewChain(base, "resp-call", []json.RawMessage{json.RawMessage(`{"type":"function_call","call_id":"expected","name":"lookup","arguments":"{}"}`)})
	assistant := `{"role":"assistant","content":[{"type":"tool_use","id":"different","name":"lookup","input":{}}]}`
	next := wsRequest(t, wsBody(wsUser("call")+`,`+assistant, controls))
	if _, ok, reason := chain.Delta(next); ok || reason != "tool call changed" {
		t.Fatalf("mismatched call id Delta() = %v, %q; want tool call changed", ok, reason)
	}
}

func TestWSChainRejectsMissingKeysAndIDs(t *testing.T) {
	base := wsRequest(t, wsBody(wsUser("base"), ""))
	if NewChain(base, "", nil) == nil {
		t.Fatal("keyed request did not produce a chain")
	}
	unkeyed := wsRequest(t, `{"model":"claude-test","messages":[{"role":"user","content":"base"}]}`)
	if chain := NewChain(unkeyed, "resp", nil); chain != nil {
		t.Fatal("request without cache key produced a chain")
	}
	if _, ok, reason := (*WSChain)(nil).Delta(base); ok || reason != "no chain" {
		t.Fatalf("nil chain Delta() = %v, %q", ok, reason)
	}
	if _, ok, reason := NewChain(base, "", nil).Delta(base); ok || reason != "no response id" {
		t.Fatalf("empty response id Delta() = %v, %q", ok, reason)
	}
	if _, ok, reason := NewChain(base, "resp", nil).Delta(unkeyed); ok || reason != "no cache key" {
		t.Fatalf("missing key Delta() = %v, %q", ok, reason)
	}
	changedKey := *base
	changedKey.PromptCacheKey = "different"
	if _, ok, reason := NewChain(base, "resp", nil).Delta(&changedKey); ok || reason != "cache key changed" {
		t.Fatalf("different key Delta() = %v, %q", ok, reason)
	}
	chain := NewChain(base, "resp", nil)
	modelChanged := *base
	modelChanged.Model = "gpt-5.1"
	if _, ok, reason := chain.Delta(&modelChanged); ok || reason != "model changed" {
		t.Fatalf("changed model Delta() = %v, %q", ok, reason)
	}
	if chain.ResponseID() != "resp" || NewChain(base, "id", nil).ResponseID() != "id" {
		t.Fatal("ResponseID did not retain the response ID")
	}
	if _, err := WSCreatePayload(nil, "", nil); err == nil {
		t.Fatal("nil request unexpectedly marshaled")
	}
}

func TestWSChainSnapshotsNestedInputOutputAndDelta(t *testing.T) {
	original := map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "base"}}}
	req := &Request{Model: "gpt-5", PromptCacheKey: "key", Input: []any{original}, ToolChoice: "auto"}
	raw := json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`)
	chain := NewChain(req, "resp", []json.RawMessage{raw})
	original["role"] = "assistant"
	original["content"].([]any)[0].(map[string]any)["text"] = "changed"
	raw[0] = 'x'
	if chain.inputLen != 1 {
		t.Fatalf("chain input length = %d, want 1", chain.inputLen)
	}
	if !json.Valid(chain.output[0]) || !bytes.Contains(chain.output[0], []byte(`"answer"`)) {
		t.Fatalf("chain output snapshot changed: %s", chain.output[0])
	}
	next := &Request{Model: "gpt-5", PromptCacheKey: "key", ToolChoice: "auto", Input: []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "base"}}},
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "answer"}}},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "next"}}},
	}}
	got, ok, reason := chain.Delta(next)
	if !ok || reason != "" || len(got) != 1 {
		t.Fatalf("snapshot Delta() = %#v, %v, %q", got, ok, reason)
	}
	got[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] = "mutated delta"
	if next.Input[2].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "next" {
		t.Fatal("returned delta aliases the request input")
	}
}

func TestWSCreatePayloadMatchesHTTPFieldsWithoutMutation(t *testing.T) {
	req := wsRequest(t, wsBody(wsUser("question"), `"system":"system","tools":[{"name":"lookup","input_schema":{"type":"object"}}],`))
	input := []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "delta"}}}}
	beforeReq, _ := json.Marshal(req)
	beforeInput, _ := json.Marshal(input)
	got, err := WSCreatePayload(req, "resp-prev", input)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	var wantReq map[string]json.RawMessage
	if err := json.Unmarshal(beforeReq, &wantReq); err != nil {
		t.Fatal(err)
	}
	wantReq["type"] = json.RawMessage(`"response.create"`)
	wantReq["input"] = mustRawJSON(t, input)
	wantReq["stream"] = json.RawMessage("true")
	wantReq["previous_response_id"] = json.RawMessage(`"resp-prev"`)
	want, _ := json.Marshal(wantReq)
	var normalizedGot, normalizedWant any
	if err := json.Unmarshal(got, &normalizedGot); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &normalizedWant); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalizedGot, normalizedWant) {
		t.Fatalf("payload = %s, want HTTP parity %s", got, want)
	}
	if _, exists := payload["previous_response_id"]; !exists {
		t.Fatal("previous_response_id missing")
	}
	noPrevious, err := WSCreatePayload(req, "", input)
	if err != nil {
		t.Fatal(err)
	}
	var noPreviousObject map[string]json.RawMessage
	if err := json.Unmarshal(noPrevious, &noPreviousObject); err != nil {
		t.Fatal(err)
	}
	if _, exists := noPreviousObject["previous_response_id"]; exists {
		t.Fatal("empty previous response id was serialized")
	}
	afterReq, _ := json.Marshal(req)
	afterInput, _ := json.Marshal(input)
	if !bytes.Equal(beforeReq, afterReq) || !bytes.Equal(beforeInput, afterInput) {
		t.Fatal("payload construction mutated request or caller input")
	}
}

func mustRawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWSChainDoesNotSkipAssistantTurnWithoutMatchingOutput(t *testing.T) {
	base := wsRequest(t, wsBody(wsUser("question"), ""))
	chain := NewChain(base, "resp", nil)
	next := wsRequest(t, wsBody(wsUser("question")+`,{"role":"assistant","content":"not verified"},`+wsUser("next"), ""))
	if _, ok, reason := chain.Delta(next); ok || reason != "output mismatch" {
		t.Fatalf("unverified assistant turn Delta() = %v, %q; want output mismatch", ok, reason)
	}
}

func TestWSChainUsesDistinctControlFailureReasons(t *testing.T) {
	base := &Request{Model: "model", PromptCacheKey: "key", Input: []any{"history"}, Tools: []Tool{{Name: "a"}}, Reasoning: &Reasoning{Effort: "low"}, ToolChoice: "auto"}
	chain := NewChain(base, "id", nil)
	chain.fingerprint.Items = 300
	nextWithOneNew := *base
	nextWithOneNew.Input = append(append([]any(nil), base.Input...), map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "new"}}})
	if _, ok, reason := chain.Delta(&nextWithOneNew); !ok || reason != "" {
		t.Fatalf("fingerprint item count alone blocked delta: ok=%v reason=%q", ok, reason)
	}
	chain = NewChain(base, "id", nil)
	toolsChanged := *base
	toolsChanged.Tools = append(append([]Tool(nil), base.Tools...), Tool{Name: "b"})
	if _, ok, reason := chain.Delta(&toolsChanged); ok || reason != "tools changed" {
		t.Fatalf("tools changed Delta() = %v, %q", ok, reason)
	}
	checks := []struct {
		name string
		edit func(*Request)
		want string
	}{
		{"model", func(r *Request) { r.Model = "other" }, "model changed"},
		{"tools", func(r *Request) { r.Tools = append(r.Tools, Tool{Name: "b"}) }, "tools changed"},
		{"system", func(r *Request) {
			r.Input = []any{messageItem{Type: "message", Role: "developer", Content: []contentPart{{Type: "input_text", Text: "system"}}}, "history"}
		}, "system changed"},
		{"reasoning", func(r *Request) { r.Reasoning = &Reasoning{Effort: "high"} }, "reasoning changed"},
		{"tool choice", func(r *Request) { r.ToolChoice = "none" }, "tool choice changed"},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			changed := *base
			changed.Input = append([]any(nil), base.Input...)
			changed.Tools = append([]Tool(nil), base.Tools...)
			check.edit(&changed)
			if _, ok, reason := chain.Delta(&changed); ok || reason != check.want {
				t.Fatalf("Delta() = %v, %q; want %q", ok, reason, check.want)
			}
		})
	}
}
