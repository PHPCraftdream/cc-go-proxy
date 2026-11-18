package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"reflect"
	"regexp"
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
	if got := gotReq.Input[0].(MessageItem).Content[0].Text; got != wantIdentity {
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
	msg, ok := req.Input[0].(MessageItem)
	if !ok || msg.Role != "developer" || len(msg.Content) != 1 {
		t.Fatalf("input item = %#v, want one developer text message", req.Input[0])
	}
	return msg.Content[0].Text
}

type requestGolden struct {
	Name       string          `json:"name"`
	Output     json.RawMessage `json:"output"`
	Error      string          `json:"error"`
	WantStream bool            `json:"want_stream"`
}

type requestGoldenInput struct {
	name       string
	body       string
	model      string
	cacheScope string
	wantStream bool
	wantError  string
}

var requestGoldenInputs = []requestGoldenInput{
	{
		name:  "string-content",
		body:  `{"model":"claude-3-sonnet","messages":[{"role":"user","content":"hello from a string"}],"stream":true}`,
		model: "gpt-5", cacheScope: "profile-a", wantStream: true,
	},
	{
		name:  "array-empty-and-null-content",
		body:  `{"model":"claude-3-haiku","messages":[{"role":"user","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]},{"role":"user","content":[]},{"role":"user","content":null}]}`,
		model: "gpt-5", cacheScope: "profile-a",
	},
	{
		name:  "tool-use-raw-whitespace-numbers",
		body:  `{"model":"claude-3-opus","messages":[{"role":"assistant","content":[{"type":"text","text":"before"},{"type":"tool_use","id":"call-1","name":"lookup","input":{ "z" : 1.2300e+04, "a" : [ 3, 2 ] }},{"type":"tool_use","id":"call-2","name":"empty","input":null}]}]}`,
		model: "gpt-5", cacheScope: "profile-a",
	},
	{
		name:  "tool-results-string-array-null",
		body:  `{"model":"claude-3-sonnet","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-str","content":"plain result"},{"type":"tool_result","tool_use_id":"call-array","content":[{"type":"text","text":"first"},{"type":"image","source":{"type":"url","url":"https://example.invalid/image"}},{"type":"text","text":"second"}]},{"type":"tool_result","tool_use_id":"call-null","content":null}]}]}`,
		model: "gpt-5", cacheScope: "profile-a",
	},
	{
		name:  "image-and-unsupported-blocks",
		body:  `{"model":"claude-3-sonnet","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AQID"}},{"type":"image","source":{"type":"url","url":"https://example.invalid/photo.png"}},{"type":"image","source":{"type":"file","data":"ignored"}},{"type":"document","title":"ignored"},{"type":"audio","data":"ignored"},{"type":"text","text":"visible"}]}]}`,
		model: "gpt-5", cacheScope: "profile-a",
	},
	{
		name:  "identity-system-user-assistant-tool-result",
		body:  `{"model":"claude-sonnet-4","system":"You are powered by the model named Sonnet 4. The exact model ID is claude-sonnet-4.\nAssistant knowledge cutoff is May 2025.\nKeep this.","messages":[{"role":"user","content":[{"type":"text","text":"You are powered by the model named Sonnet 4. The exact model ID is claude-sonnet-4.\nKeep this.","z":1.00,"a":"</tag>"},{"type":"tool_result","tool_use_id":"call-id","content":[{"z":1.00,"type":"text","text":"You are powered by the model named Sonnet 4. The exact model ID is claude-sonnet-4.","a":"</tag>"}]}]},{"role":"assistant","content":[{"type":"text","text":"You are powered by the model named Sonnet 4. The exact model ID is claude-sonnet-4."}]}]}`,
		model: "gpt-5", cacheScope: "profile-a",
	},
	{
		name:  "identity-system-array",
		body:  `{"model":"claude-sonnet-4","system":[{"type":"text","text":"You are powered by the model named Sonnet 4. The exact model ID is claude-sonnet-4."},{"type":"text","text":"Preserve this."}],"messages":[]}`,
		model: "gpt-5", cacheScope: "profile-a",
	},
	{
		name:  "null-system",
		body:  `{"model":"claude-3-sonnet","system":null,"messages":[]}`,
		model: "gpt-5", cacheScope: "profile-a",
	},
	{
		name:  "escaped-identity-marker-not-detected-by-raw-gate",
		body:  `{"model":"claude-sonnet-4","messages":[{"role":"user","content":[{"type":"text","text":"You are powered\u0020by the model named Sonnet 4. The exact model ID is claude-sonnet-4.","n":1.00,"z":"</tag>"}]}]}`,
		model: "gpt-5", cacheScope: "profile-a",
	},
	{
		name:  "thinking-and-control-tools-metadata",
		body:  `{"model":"claude-3-opus","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"retain this summary","signature":"ccgp1:encrypted-state"},{"type":"thinking","thinking":"","signature":"ccgp1:empty-state"},{"type":"thinking","thinking":"ignore this","signature":"other:state"},{"type":"text","text":"answer"}]}],"stream":false,"thinking":{"type":"enabled","budget_tokens":20000},"tool_choice":{"type":"tool","name":"lookup"},"tools":[{"type":"custom","name":"lookup","description":"Find a record","input_schema":{ "type" : "object", "properties" : { "count" : { "type" : "number" } } }},{"type":"web_search_20250305","name":"web_search","input_schema":{"type":"object"}},{"type":"custom","name":"no_schema"}],"metadata":{"user_id":"user-17","tag":"ignored"}}`,
		model: "gpt-5", cacheScope: "session-scope",
	},
	{
		name:  "tool-choice-any",
		body:  `{"model":"claude-3-haiku","messages":[],"tool_choice":{"type":"any"},"stream":true}`,
		model: "gpt-5", cacheScope: "profile-a", wantStream: true,
	},
	{
		name:  "tool-choice-none",
		body:  `{"model":"claude-3-haiku","messages":[],"tool_choice":{"type":"none"},"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`,
		model: "gpt-5", cacheScope: "profile-a",
	},
	{
		name:  "invalid-json",
		body:  `{"model":"claude-3","messages":[`,
		model: "gpt-5", cacheScope: "profile-a", wantError: "invalid request body: unexpected end of JSON input",
	},
	{
		name:  "invalid-content-object",
		body:  `{"model":"claude-3","messages":[{"role":"user","content":{"type":"text","text":"not an array"}}]}`,
		model: "gpt-5", cacheScope: "profile-a", wantError: "messages[0]: content: json: cannot unmarshal object into Go value of type []codex.anthropicBlock",
	},
	{
		name:  "invalid-content-number",
		body:  `{"model":"claude-3","messages":[{"role":"assistant","content":7}]}`,
		model: "gpt-5", cacheScope: "profile-a", wantError: "messages[0]: content: json: cannot unmarshal number into Go value of type []codex.anthropicBlock",
	},
}

func TestConvertRequestGoldenCorpus(t *testing.T) {
	file, err := os.Open("testdata/request_conversion.golden.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	goldens := make(map[string]requestGolden, len(requestGoldenInputs))
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var golden requestGolden
		if err := json.Unmarshal(scanner.Bytes(), &golden); err != nil {
			t.Fatalf("decode golden line: %v", err)
		}
		if _, exists := goldens[golden.Name]; exists {
			t.Fatalf("duplicate golden %q", golden.Name)
		}
		goldens[golden.Name] = golden
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(goldens) != len(requestGoldenInputs) {
		t.Fatalf("golden count = %d, input count = %d", len(goldens), len(requestGoldenInputs))
	}

	for _, input := range requestGoldenInputs {
		t.Run(input.name, func(t *testing.T) {
			golden, ok := goldens[input.name]
			if !ok {
				t.Fatalf("missing golden %q", input.name)
			}
			got, wantStream, err := ConvertRequest([]byte(input.body), input.model, input.cacheScope)
			parsed, parseErr := ParseRequest([]byte(input.body))
			if input.wantError != "" {
				if err == nil || err.Error() != input.wantError {
					t.Fatalf("error = %v, want %q", err, input.wantError)
				}
				if parseErr == nil {
					_, _, parsedErr := parsed.Convert(input.model, input.cacheScope)
					if parsedErr == nil || parsedErr.Error() != input.wantError {
						t.Fatalf("parsed conversion error = %v, want %q", parsedErr, input.wantError)
					}
				} else if !strings.HasPrefix(input.wantError, "invalid request body:") || parseErr.Error() != input.wantError {
					t.Fatalf("parse error = %v, want %q", parseErr, input.wantError)
				}
				if golden.Error != input.wantError || string(golden.Output) != "null" {
					t.Fatalf("golden error/output = %q/%s, want %q/no output", golden.Error, golden.Output, input.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			originalModel := ""
			var modelProbe struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal([]byte(input.body), &modelProbe); err != nil {
				t.Fatal(err)
			}
			originalModel = modelProbe.Model
			if parsed.Model() != originalModel {
				t.Fatalf("parsed model = %q, want %q", parsed.Model(), originalModel)
			}
			parsedGot, parsedWantStream, err := parsed.Convert(input.model, input.cacheScope)
			if err != nil {
				t.Fatalf("parsed conversion: %v", err)
			}
			if parsedWantStream != wantStream {
				t.Fatalf("parsed wantStream = %v, ConvertRequest wantStream = %v", parsedWantStream, wantStream)
			}
			parsedBytes, err := json.Marshal(parsedGot)
			if err != nil {
				t.Fatal(err)
			}
			gotBytes, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(parsedBytes, gotBytes) {
				t.Errorf("json.Marshal(parsed.Convert) differs from ConvertRequest\\n got: %s\\nwant: %s", parsedBytes, gotBytes)
			}
			if !bytes.Equal(parsedBytes, golden.Output) {
				t.Errorf("json.Marshal(parsed.Convert) mismatch\\n got: %s\\nwant: %s", parsedBytes, golden.Output)
			}
			if golden.Error != "" {
				t.Fatalf("unexpected golden error %q", golden.Error)
			}
			if wantStream != input.wantStream || golden.WantStream != input.wantStream {
				t.Fatalf("wantStream = %v, fixture = %v, want %v", wantStream, golden.WantStream, input.wantStream)
			}
			if !bytes.Equal(gotBytes, golden.Output) {
				t.Errorf("json.Marshal(ConvertRequest) mismatch\n got: %s\nwant: %s", gotBytes, golden.Output)
			}
		})
	}
}

func BenchmarkConvertRequestLargeConversation(b *testing.B) {
	const (
		targetBodyBytes = 5 << 20
		messageCount    = 1500
	)
	body := largeConversationBody(targetBodyBytes, messageCount)
	const model, cacheScope = "gpt-5", "benchmark-session"
	req, _, err := ConvertRequest(body, model, cacheScope)
	if err != nil {
		b.Fatal(err)
	}
	if len(req.Input) != messageCount {
		b.Fatalf("converted item count = %d, want %d", len(req.Input), messageCount)
	}

	b.Run("conversion_and_marshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(body)))
		b.ReportMetric(float64(len(body)), "body-B")
		b.ReportMetric(float64(messageCount), "items/op")
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			req, _, err := ConvertRequest(body, model, cacheScope)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := json.Marshal(req); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("parsed_ingress_convert_marshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(body)))
		b.ReportMetric(float64(len(body)), "body-B")
		b.ReportMetric(float64(messageCount), "items/op")
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			parsed, err := ParseRequest(body)
			if err != nil {
				b.Fatal(err)
			}
			req, _, err := parsed.Convert(model, cacheScope)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := json.Marshal(req); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("ingress_copy_probe_convert_marshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(body)))
		b.ReportMetric(float64(len(body)), "body-B")
		b.ReportMetric(float64(messageCount), "items/op")
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ownedBody := append([]byte(nil), body...)
			var probe struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(ownedBody, &probe); err != nil {
				b.Fatal(err)
			}
			req, _, err := ConvertRequest(ownedBody, model, cacheScope)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := json.Marshal(req); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func largeConversationBody(targetBytes, messageCount int) []byte {
	const marker = "You are powered by the model named Claude 3. The exact model ID is claude-3."
	build := func(payload string) []byte {
		var body strings.Builder
		body.Grow(targetBytes)
		body.WriteString(`{"model":"claude-3","messages":[`)
		for i := 0; i < messageCount-1; i++ {
			if i > 0 {
				body.WriteByte(',')
			}
			fmt.Fprintf(&body, `{"role":"user","content":"message-%04d"}`, i)
		}
		if messageCount > 1 {
			body.WriteByte(',')
		}
		body.WriteString(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"large-result","content":[{"type":"text","text":"`)
		body.WriteString(payload)
		body.WriteString(`"}]}]}`)
		body.WriteString(`]}`)
		return []byte(body.String())
	}
	empty := build("")
	filler := targetBytes - len(empty) - len(marker)
	if filler < 0 {
		panic("target body is too small")
	}
	left := filler / 2
	payload := strings.Repeat("x", left) + marker + strings.Repeat("y", filler-left)
	body := build(payload)
	if len(body) != targetBytes {
		panic(fmt.Sprintf("large body = %d bytes, want %d", len(body), targetBytes))
	}
	return body
}

func fpBody(system, tools, userID string, messages int) string {
	var msgs []string
	for i := range messages {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, fmt.Sprintf(`{"role":%q,"content":"secret message %d"}`, role, i))
	}
	sys := ""
	if system != "" {
		sys = fmt.Sprintf(`"system":[{"type":"text","text":%q}],`, system)
	}
	tl := ""
	if tools != "" {
		tl = fmt.Sprintf(`"tools":[{"name":%q,"description":"secret tool description","input_schema":{"type":"object"}}],`, tools)
	}
	meta := ""
	if userID != "" {
		meta = fmt.Sprintf(`"metadata":{"user_id":%q},`, userID)
	}
	return fmt.Sprintf(`{"model":"claude-sonnet-5-5",%s%s%s"messages":[%s]}`, sys, tl, meta, strings.Join(msgs, ","))
}

func fingerprintOf(t *testing.T, body string) Fingerprint {
	t.Helper()
	req, _, err := ConvertRequest([]byte(body), "gpt-5", "work")
	if err != nil {
		t.Fatal(err)
	}
	return NewFingerprint(req)
}

func TestFingerprintOfARequest(t *testing.T) {
	f := fingerprintOf(t, fpBody("You are Claude Code.", "Read", "user_1_session_2", 3))
	key, system, tools := regexp.MustCompile(`^[0-9a-f]{32}$`), regexp.MustCompile(`^[0-9a-f]{8}$`), regexp.MustCompile(`^[0-9a-f]{8}$`)
	for name, v := range map[string]string{"key": f.Key, "system": f.System, "tools": f.Tools} {
		valid := system.MatchString(v)
		if name == "key" {
			valid = key.MatchString(v)
		}
		if name == "tools" {
			valid = tools.MatchString(v)
		}
		if !valid {
			t.Errorf("%s = %q, want the expected hexadecimal digits", name, v)
		}
	}
	// The system prompt is the first input item, then the three messages.
	if f.NTools != 1 || f.Items != 4 {
		t.Errorf("tools = %d, items = %d; want 1 and 4", f.NTools, f.Items)
	}
	req, _, _ := ConvertRequest([]byte(fpBody("You are Claude Code.", "Read", "user_1_session_2", 3)), "gpt-5", "work")
	if f.Key != req.PromptCacheKey {
		t.Errorf("key = %q, want full cache key %q", f.Key, req.PromptCacheKey)
	}
	want := fmt.Sprintf("key %s, system %s, 1 tools %s, 4 input items", f.Key[:8], f.System, f.Tools)
	if got := f.String(); got != want || strings.Contains(got, f.Key) {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestFingerprintIsStableAndSeesWhatChanges(t *testing.T) {
	base := fingerprintOf(t, fpBody("sys A", "Read", "u", 3))
	if again := fingerprintOf(t, fpBody("sys A", "Read", "u", 3)); again != base {
		t.Errorf("the same request has two fingerprints: %+v and %+v", base, again)
	}
	grown := fingerprintOf(t, fpBody("sys A", "Read", "u", 5))
	if grown.System != base.System || grown.Tools != base.Tools || grown.Key != base.Key || grown.Items != base.Items+2 {
		t.Errorf("a longer history changes only the count: %+v vs %+v", grown, base)
	}
	if other := fingerprintOf(t, fpBody("sys B", "Read", "u", 3)); other.System == base.System || other.Tools != base.Tools || other.Key != base.Key {
		t.Errorf("another system prompt: %+v vs %+v", other, base)
	}
	if other := fingerprintOf(t, fpBody("sys A", "Write", "u", 3)); other.Tools == base.Tools || other.System != base.System || other.Key != base.Key {
		t.Errorf("another tool: %+v vs %+v", other, base)
	}
	if other := fingerprintOf(t, fpBody("sys A", "Read", "v", 3)); other.Key == base.Key || other.System != base.System {
		t.Errorf("another session: %+v vs %+v", other, base)
	}
}

func TestFingerprintIncludesBackendModelAndControlsIndependentlyOfHistory(t *testing.T) {
	base := Request{
		Model:      "gpt-5-codex",
		Input:      []any{"first history"},
		Reasoning:  &Reasoning{Effort: "high", Summary: "auto"},
		ToolChoice: map[string]any{"type": "function", "name": "secret_tool"},
	}
	f := NewFingerprint(&base)
	if f.Model != base.Model || f.Reasoning == "" || f.ToolChoice == "" {
		t.Fatalf("fingerprint did not capture backend model and controls: %+v", f)
	}
	if len(f.Reasoning) != 8 || len(f.ToolChoice) != 8 {
		t.Errorf("control hashes are not short hashes: reasoning=%q tool choice=%q", f.Reasoning, f.ToolChoice)
	}
	if got := NewFingerprint(&Request{Model: base.Model, Input: []any{"different history"}, Reasoning: base.Reasoning, ToolChoice: base.ToolChoice}); got.Model != f.Model || got.Reasoning != f.Reasoning || got.ToolChoice != f.ToolChoice {
		t.Errorf("history changed independent settings: %+v vs %+v", got, f)
	}
	line := f.String()
	for _, secret := range []string{"high", "auto", "secret_tool", "function"} {
		if strings.Contains(line, secret) {
			t.Errorf("control content %q is exposed: %s", secret, line)
		}
	}

	cases := []struct {
		name string
		edit func(*Request)
	}{
		{"model", func(r *Request) { r.Model = "gpt-5.1-codex" }},
		{"reasoning effort", func(r *Request) { r.Reasoning = &Reasoning{Effort: "low", Summary: "auto"} }},
		{"reasoning summary", func(r *Request) { r.Reasoning = &Reasoning{Effort: "high", Summary: "detailed"} }},
		{"tool choice", func(r *Request) { r.ToolChoice = map[string]any{"type": "function", "name": "another_secret_tool"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := base
			tc.edit(&changed)
			got := NewFingerprint(&changed)
			if got == f {
				t.Errorf("changing %s did not change the fingerprint", tc.name)
			}
		})
	}
}

func TestFingerprintOfARequestWithoutParts(t *testing.T) {
	f := fingerprintOf(t, fpBody("", "", "", 1))
	if f.Key != "" || f.System != "" || f.Tools != "" || f.Model != "gpt-5" || f.Reasoning == "" || f.ToolChoice == "" || f.NTools != 0 || f.Items != 1 {
		t.Errorf("fingerprint = %+v", f)
	}
	if got, want := f.String(), "no cache key, no system prompt, no tools, 1 input items"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got := NewFingerprint(&Request{}); got != (Fingerprint{}) {
		t.Errorf("an empty request has the fingerprint %+v", got)
	}
	key := "01234567same-prefix-but-not-rendered"
	f = NewFingerprint(&Request{PromptCacheKey: key})
	if f.Key != key {
		t.Errorf("fingerprint key = %q, want %q", f.Key, key)
	}
	if got := f.String(); !strings.Contains(got, "key 01234567,") || strings.Contains(got, key) {
		t.Errorf("String() = %q; want only the display prefix", got)
	}
}

// P0: the log gets hashes and counts. Not a word of the prompt, the tools or the
// messages, and not the id the cache key comes from.
func TestFingerprintHoldsNothingOfThePrompt(t *testing.T) {
	body := fpBody("You are Claude Code. TOP-SECRET-SYSTEM-TEXT", "SecretToolName", "user_account_SECRET_ID", 4)
	text := fingerprintOf(t, body).String()
	for _, secret := range []string{"TOP-SECRET-SYSTEM-TEXT", "SecretToolName", "secret tool description", "secret message", "SECRET_ID", "user_account", "Claude Code"} {
		if strings.Contains(text, secret) {
			t.Errorf("the fingerprint line holds %q: %s", secret, text)
		}
	}
	req, _, _ := ConvertRequest([]byte(body), "gpt-5", "work")
	if strings.Contains(text, req.PromptCacheKey) {
		t.Errorf("the whole cache key is in the line: %s", text)
	}
}

// A system prompt is the first input item only when the request has one: a
// first message of the user must not be taken for it.
func TestFingerprintSystemIsNotTheFirstMessage(t *testing.T) {
	f := fingerprintOf(t, fpBody("", "Read", "u", 2))
	if f.System != "" {
		t.Errorf("System = %q for a request without a system prompt", f.System)
	}
	if f.Items != 2 {
		t.Errorf("items = %d, want 2", f.Items)
	}
}

func hashesOf(t *testing.T, body string) []uint64 {
	t.Helper()
	req, _, err := ConvertRequest([]byte(body), "gpt-5", "work")
	if err != nil {
		t.Fatal(err)
	}
	return ItemHashes(req)
}

func TestItemHashes(t *testing.T) {
	base := hashesOf(t, fpBody("sys", "Read", "u", 4))
	if len(base) != 5 {
		t.Fatalf("%d hashes, want 5 (system prompt and 4 messages)", len(base))
	}
	again := hashesOf(t, fpBody("sys", "Read", "u", 4))
	for i := range base {
		if base[i] != again[i] {
			t.Errorf("item %d hashes differently for the same request", i)
		}
	}
	seen := map[uint64]bool{}
	for _, h := range base {
		if seen[h] {
			t.Errorf("two different items share the hash %d", h)
		}
		seen[h] = true
	}
	// A longer history keeps the hashes of the items it had.
	grown := hashesOf(t, fpBody("sys", "Read", "u", 6))
	if len(grown) != 7 {
		t.Fatalf("%d hashes, want 7", len(grown))
	}
	for i := range base {
		if grown[i] != base[i] {
			t.Errorf("item %d changed when only the tail grew", i)
		}
	}
	// Another system prompt changes item 0 only.
	other := hashesOf(t, fpBody("sys 2", "Read", "u", 4))
	if other[0] == base[0] {
		t.Error("the system prompt changed but item 0 did not")
	}
	for i := 1; i < len(base); i++ {
		if other[i] != base[i] {
			t.Errorf("item %d changed with the system prompt", i)
		}
	}
}

func TestItemHashesOfAnEmptyRequest(t *testing.T) {
	if got := ItemHashes(&Request{}); got != nil {
		t.Errorf("hashes of an empty request: %#v, want nil", got)
	}
}

func TestNewDigestMatchesThePiecewiseHashes(t *testing.T) {
	req := &Request{
		Model:          "gpt-5-codex",
		PromptCacheKey: "digest-cache-key",
		Input: []any{
			MessageItem{Type: "message", Role: "developer", Content: []ContentPart{{Type: "input_text", Text: "system prompt"}}},
			MessageItem{Type: "message", Role: "user", Content: []ContentPart{{Type: "input_text", Text: "first"}}},
			MessageItem{Type: "message", Role: "assistant", Content: []ContentPart{{Type: "input_text", Text: "answer"}}},
			MessageItem{Type: "message", Role: "user", Content: []ContentPart{{Type: "input_text", Text: "second"}}},
		},
		Tools:      []Tool{{Type: "function", Name: "secret_tool", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Reasoning:  &Reasoning{Effort: "high", Summary: "auto"},
		ToolChoice: map[string]any{"type": "function", "name": "secret_tool"},
	}
	d := NewDigest(req)
	if d.Fingerprint != NewFingerprint(req) {
		t.Errorf("digest fingerprint = %+v, want %+v", d.Fingerprint, NewFingerprint(req))
	}
	want := ItemHashes(req)
	got := d.Hashes()
	if len(got) != len(want) {
		t.Fatalf("%d hashes, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d hash = %d, want %d", i, got[i], want[i])
		}
	}
	if len(got) != len(req.Input) {
		t.Errorf("%d hashes for %d input items", len(got), len(req.Input))
	}

	if got := NewDigest(&Request{Input: []any{MessageItem{Type: "message", Role: "user"}}}).Hashes(); got != nil {
		t.Errorf("hashes without a cache key: %#v, want nil", got)
	}
	if got := (*Digest)(nil).Hashes(); got != nil {
		t.Errorf("hashes of a nil digest: %#v, want nil", got)
	}
}

type countingMarshaler struct {
	calls *int
}

func (m countingMarshaler) MarshalJSON() ([]byte, error) {
	*m.calls++
	return []byte(`{"value":"marshaled"}`), nil
}

func TestItemHashesWithoutKeyDoesNotMarshal(t *testing.T) {
	calls := 0
	req := &Request{Input: []any{countingMarshaler{calls: &calls}}}
	if got := ItemHashes(req); got != nil {
		t.Errorf("hashes without a cache key: %#v, want nil", got)
	}
	if calls != 0 {
		t.Errorf("MarshalJSON called %d times without a cache key, want 0", calls)
	}
}

type failingMarshaler struct{}

func (failingMarshaler) MarshalJSON() ([]byte, error) {
	return nil, errors.New("deliberate marshal failure")
}

func TestItemHashesMatchMarshalReference(t *testing.T) {
	items := []any{
		`quotes: " slash: \\ newline:\n tab:\t <tag>`,
		json.RawMessage(`{"raw":"quote\\\" and newline\\n","number":1}`),
		map[string]any{"z": "last", "a": []any{true, nil, `/<&`}},
		failingMarshaler{},
		map[string]any{"after_error": `still hashes`, "escaped": "\u2028\u2029"},
	}
	req := &Request{PromptCacheKey: "key", Input: items}
	got := ItemHashes(req)
	if len(got) != len(items) {
		t.Fatalf("got %d hashes, want %d", len(got), len(items))
	}
	for i, item := range items {
		b, err := json.Marshal(item)
		if err != nil {
			if got[i] != 0 {
				t.Errorf("item %d hash = %d after marshal error, want zero", i, got[i])
			}
			continue
		}
		h := fnv.New64a()
		_, _ = h.Write(b)
		if got[i] != h.Sum64() {
			t.Errorf("item %d hash = %d, want json.Marshal reference %d", i, got[i], h.Sum64())
		}
	}
}

func BenchmarkItemHashes(b *testing.B) {
	const itemBytes = 1 << 20
	input := []any{
		MessageItem{Type: "message", Role: "user", Content: []ContentPart{{Type: "input_text", Text: strings.Repeat("A realistic prompt with escaped \\\" text and ordinary content. ", itemBytes/64)}}},
		MessageItem{Type: "message", Role: "assistant", Content: []ContentPart{{Type: "input_text", Text: strings.Repeat("A realistic answer with ordinary content and a newline.\\n", itemBytes/58)}}},
	}
	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "keyed", key: "benchmark-cache-key"},
		{name: "unkeyed"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			req := &Request{PromptCacheKey: tc.key, Input: input}
			b.ReportAllocs()
			b.SetBytes(int64(2 * itemBytes))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = ItemHashes(req)
			}
		})
	}
}
