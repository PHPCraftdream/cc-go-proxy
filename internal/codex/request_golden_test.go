package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

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
