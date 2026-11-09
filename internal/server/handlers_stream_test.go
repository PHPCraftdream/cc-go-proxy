package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

func TestStreamOpenAIToClaudeReasoningShapes(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
	}{
		{
			name: "reasoning_content",
			chunks: []string{
				`{"choices":[{"delta":{"reasoning_content":""}}]}`,
				`{"choices":[{"delta":{"reasoning_content":"first "}}]}`,
				`{"choices":[{"delta":{"reasoning_content":"second"}}]}`,
			},
		},
		{
			name: "reasoning_details text and summary",
			chunks: []string{
				`{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.encrypted","data":"secret"}]}}]}`,
				`{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.text","text":"first "}]}}]}`,
				`{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.summary","summary":"second"}]}}]}`,
			},
		},
		{
			name: "reasoning string",
			chunks: []string{
				`{"choices":[{"delta":{"reasoning":""}}]}`,
				`{"choices":[{"delta":{"reasoning":"first "}}]}`,
				`{"choices":[{"delta":{"reasoning":"second"}}]}`,
			},
		},
	}

	want := []streamEvent{
		{
			name: "content_block_start",
			data: map[string]interface{}{
				"type":          "content_block_start",
				"index":         float64(0),
				"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
			},
		},
		{
			name: "content_block_delta",
			data: map[string]interface{}{
				"type":  "content_block_delta",
				"index": float64(0),
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": "first "},
			},
		},
		{
			name: "content_block_delta",
			data: map[string]interface{}{
				"type":  "content_block_delta",
				"index": float64(0),
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": "second"},
			},
		},
	}
	wantClosed := append(append([]streamEvent(nil), want...), streamEvent{
		name: "content_block_stop",
		data: map[string]interface{}{"type": "content_block_stop", "index": float64(0)},
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var input strings.Builder
			for _, chunk := range tt.chunks {
				input.WriteString("data: ")
				input.WriteString(chunk)
				input.WriteString("\n\n")
			}
			reader := &reasoningGateReader{
				data:     []byte(input.String()),
				terminal: []byte("data: [DONE]\n\n"),
				blocked:  make(chan struct{}),
				release:  make(chan struct{}),
			}
			output := &lockedStreamOutput{}
			done := make(chan struct{})
			go func() {
				streamOpenAIToClaude(bufio.NewWriter(output), reader, "test-model", &config.Config{}, time.Now())
				close(done)
			}()
			defer reader.allowEOF()

			select {
			case <-reader.blocked:
			case <-time.After(2 * time.Second):
				t.Fatal("stream did not wait for input after reasoning chunks")
			}

			gotBeforeEOF := reasoningBlockEvents(output.String())
			if !reflect.DeepEqual(gotBeforeEOF, want) {
				t.Fatalf("reasoning block events before EOF = %#v, want %#v", gotBeforeEOF, want)
			}

			reader.allowEOF()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("stream did not finish after EOF")
			}
			gotClosed := reasoningBlockEvents(output.String())
			if !reflect.DeepEqual(gotClosed, wantClosed) {
				t.Errorf("reasoning block events after EOF = %#v, want %#v", gotClosed, wantClosed)
			}
			if events := parseStreamEvents(output.String()); len(events) < 2 || events[len(events)-1].name != "message_stop" || events[len(events)-2].name != "message_delta" {
				t.Errorf("final events = %#v, want message_delta then message_stop", events)
			}
		})
	}
}

type streamEvent struct {
	name string
	data map[string]interface{}
}

type lockedStreamOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *lockedStreamOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *lockedStreamOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

type reasoningGateReader struct {
	data        []byte
	terminal    []byte
	terminalOff int
	offset      int
	sent        bool
	blocked     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func (r *reasoningGateReader) Read(p []byte) (int, error) {
	if r.offset < len(r.data) {
		n := copy(p, r.data[r.offset:])
		r.offset += n
		return n, nil
	}
	if !r.sent {
		r.sent = true
		close(r.blocked)
	}
	<-r.release
	if r.terminalOff < len(r.terminal) {
		n := copy(p, r.terminal[r.terminalOff:])
		r.terminalOff += n
		return n, nil
	}
	return 0, io.EOF
}

func (r *reasoningGateReader) allowEOF() {
	r.releaseOnce.Do(func() { close(r.release) })
}

func parseStreamEvents(body string) []streamEvent {
	var events []streamEvent
	for _, eventText := range strings.Split(body, "\n\n") {
		var event streamEvent
		var data string
		for _, line := range strings.Split(eventText, "\n") {
			if strings.HasPrefix(line, "event: ") {
				event.name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if event.name != "" && data != "" {
			if err := json.Unmarshal([]byte(data), &event.data); err == nil {
				events = append(events, event)
			}
		}
	}
	return events
}

func reasoningBlockEvents(body string) []streamEvent {
	var events []streamEvent
	for _, event := range parseStreamEvents(body) {
		if strings.HasPrefix(event.name, "content_block_") && (event.data["index"] == float64(0)) {
			events = append(events, event)
		}
	}
	return events
}

func TestStreamOpenAIToClaudeRequiresTerminalMarker(t *testing.T) {
	for _, tt := range []struct {
		name   string
		reader io.Reader
	}{
		{name: "premature EOF", reader: strings.NewReader(`data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\n")},
		{name: "read error", reader: &streamReadErrorReader{data: []byte(`data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\n")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			err := streamOpenAIToClaude(bufio.NewWriter(&output), tt.reader, "test-model", &config.Config{}, time.Now())
			if err != nil {
				t.Fatalf("streamOpenAIToClaude() error = %v, want the stream error to be emitted as SSE", err)
			}
			events := parseStreamEvents(output.String())
			if len(events) == 0 || events[len(events)-1].name != "error" {
				t.Fatalf("final event = %#v, want error", events)
			}
			for _, event := range events {
				if event.name == "message_delta" || event.name == "message_stop" {
					t.Fatalf("success event %q emitted before stream error", event.name)
				}
			}
		})
	}
}

type streamReadErrorReader struct {
	data []byte
	done bool
}

func (r *streamReadErrorReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.data), nil
	}
	return 0, errors.New("upstream read failed")
}

func TestStreamOpenAIToClaudeFinishReasonTerminatesWithoutDone(t *testing.T) {
	input := `data: {"choices":[{"finish_reason":"stop"}]}` + "\n\n"
	var output bytes.Buffer
	if err := streamOpenAIToClaude(bufio.NewWriter(&output), strings.NewReader(input), "test-model", &config.Config{}, time.Now()); err != nil {
		t.Fatalf("streamOpenAIToClaude() error = %v", err)
	}
	events := parseStreamEvents(output.String())
	if len(events) < 2 || events[len(events)-2].name != "message_delta" || events[len(events)-1].name != "message_stop" {
		t.Fatalf("final events = %#v, want message_delta then message_stop", events)
	}
}

type failedStreamWriter struct {
	writes int
}

func (w *failedStreamWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, errors.New("client write failed")
}

type countedStreamReader struct {
	reads int
}

func (r *countedStreamReader) Read([]byte) (int, error) {
	r.reads++
	return 0, io.EOF
}

func TestStreamOpenAIToClaudeStopsReadingAfterWriterFailure(t *testing.T) {
	reader := &countedStreamReader{}
	writer := &failedStreamWriter{}
	err := streamOpenAIToClaude(bufio.NewWriter(writer), reader, "test-model", &config.Config{}, time.Now())
	if err == nil {
		t.Fatal("streamOpenAIToClaude() error = nil, want writer error")
	}
	if reader.reads != 0 {
		t.Errorf("upstream reads = %d, want 0 after initial flush failure", reader.reads)
	}
}

type closeTrackingBody struct {
	io.Reader
	closed bool
}

func (b *closeTrackingBody) Close() error {
	b.closed = true
	return nil
}

func TestStreamOpenAIResponseClosesBodyAndCancelsContext(t *testing.T) {
	body := &closeTrackingBody{Reader: strings.NewReader("data: [DONE]\n\n")}
	resp := &http.Response{Body: body}
	ctx, cancel := context.WithCancel(context.Background())
	if err := streamOpenAIResponse(bufio.NewWriter(io.Discard), resp, "test-model", &config.Config{}, time.Now(), cancel); err != nil {
		t.Fatalf("streamOpenAIResponse() error = %v", err)
	}
	if !body.closed {
		t.Error("response body was not closed at stream ownership boundary")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("context error = %v, want context.Canceled", ctx.Err())
	}
}

func TestStreamOpenAIToClaudeToolArgumentFragments(t *testing.T) {
	argumentCases := []struct {
		name string
		args string
	}{
		{name: "escaped characters one at a time", args: `{"text":"quote: \" slash: \\ newline: \n"}`},
		{name: "empty object", args: `{}`},
	}

	for _, tt := range argumentCases {
		t.Run(tt.name, func(t *testing.T) {
			fragments := make([]string, len(tt.args))
			for i := range tt.args {
				fragments[i] = tt.args[i : i+1]
			}
			if len(fragments) < 2 {
				t.Fatalf("test arguments %q must have multiple fragments", tt.args)
			}

			var prefix, suffix strings.Builder
			writingSuffix := false
			writeChunk := func(tcDelta map[string]interface{}, finishReason string) {
				choice := map[string]interface{}{"delta": map[string]interface{}{}}
				if tcDelta != nil {
					choice["delta"].(map[string]interface{})["tool_calls"] = []interface{}{tcDelta}
				}
				if finishReason != "" {
					choice["finish_reason"] = finishReason
				}
				chunk, err := json.Marshal(map[string]interface{}{"choices": []interface{}{choice}})
				if err != nil {
					t.Fatal(err)
				}
				line := "data: " + string(chunk) + "\n\n"
				if writingSuffix {
					suffix.WriteString(line)
				} else {
					prefix.WriteString(line)
				}
			}

			writeChunk(map[string]interface{}{
				"index": 0,
				"id":    "call_fragments",
				"function": map[string]interface{}{
					"name": "fragment_test",
				},
			}, "")
			prefixFragmentCount := len(fragments) - 1
			for _, fragment := range fragments[:prefixFragmentCount] {
				writeChunk(map[string]interface{}{
					"index":    0,
					"function": map[string]interface{}{"arguments": fragment},
				}, "")
			}
			writingSuffix = true
			for _, fragment := range fragments[prefixFragmentCount:] {
				writeChunk(map[string]interface{}{
					"index":    0,
					"function": map[string]interface{}{"arguments": fragment},
				}, "")
			}
			writeChunk(nil, "tool_calls")

			reader := &toolArgumentGateReader{
				prefix:      []byte(prefix.String()),
				suffix:      []byte(suffix.String()),
				prefixReady: make(chan struct{}),
				continueRun: make(chan struct{}),
				suffixReady: make(chan struct{}),
				eofRelease:  make(chan struct{}),
			}
			output := &lockedStreamOutput{}
			done := make(chan struct{})
			go func() {
				streamOpenAIToClaude(bufio.NewWriter(output), reader, "test-model", &config.Config{}, time.Now())
				close(done)
			}()
			defer reader.allowEOF()

			waitForSignal := func(ch <-chan struct{}, description string) {
				t.Helper()
				select {
				case <-ch:
				case <-time.After(2 * time.Second):
					t.Fatalf("stream did not reach %s", description)
				}
			}

			waitForSignal(reader.prefixReady, "the incomplete argument prefix")
			beforeCompletion := toolArgumentDeltaEvents(output.String())
			if got := toolArgumentFragments(beforeCompletion); !reflect.DeepEqual(got, fragments[:prefixFragmentCount]) {
				t.Fatalf("argument fragments before complete JSON = %#v, want %#v", got, fragments[:prefixFragmentCount])
			}
			if strings.Join(toolArgumentFragments(beforeCompletion), "") == tt.args {
				t.Fatal("all argument JSON arrived before the completion gate")
			}
			if !reflect.DeepEqual(toolArgumentIndices(beforeCompletion), repeatedIndex(beforeCompletion, 4)) {
				t.Fatalf("argument delta indices before completion = %#v", toolArgumentIndices(beforeCompletion))
			}

			close(reader.continueRun)
			waitForSignal(reader.suffixReady, "the completed arguments before EOF")
			beforeEOF := toolArgumentDeltaEvents(output.String())
			if got := toolArgumentFragments(beforeEOF); !reflect.DeepEqual(got, fragments) {
				t.Fatalf("argument fragments before EOF = %#v, want %#v", got, fragments)
			}
			if got := strings.Join(toolArgumentFragments(beforeEOF), ""); got != tt.args || !json.Valid([]byte(got)) {
				t.Fatalf("concatenated arguments before EOF = %q, want valid %q", got, tt.args)
			}

			close(reader.eofRelease)
			waitForSignal(done, "EOF completion")

			events := parseStreamEvents(output.String())
			var starts, stops []float64
			var stopReason string
			for _, event := range events {
				switch event.name {
				case "content_block_start":
					block, _ := event.data["content_block"].(map[string]interface{})
					if block["type"] == "tool_use" {
						if block["id"] != "call_fragments" || block["name"] != "fragment_test" {
							t.Errorf("tool start = %#v, want call_fragments/fragment_test", block)
						}
						starts = append(starts, event.data["index"].(float64))
					}
				case "content_block_stop":
					if event.data["index"] == float64(4) {
						stops = append(stops, event.data["index"].(float64))
					}
				case "message_delta":
					delta, _ := event.data["delta"].(map[string]interface{})
					stopReason, _ = delta["stop_reason"].(string)
				}
			}
			if !reflect.DeepEqual(starts, []float64{4}) || !reflect.DeepEqual(stops, starts) {
				t.Errorf("tool block start/stop indices = %v/%v, want [4]/[4]", starts, stops)
			}
			allDeltas := toolArgumentDeltaEvents(output.String())
			for i, event := range allDeltas {
				if event.data["index"] != float64(4) {
					t.Errorf("argument delta %d index = %v, want 4", i, event.data["index"])
				}
			}
			if stopReason != "tool_use" {
				t.Errorf("message stop reason = %q, want tool_use", stopReason)
			}
		})
	}
}

type toolArgumentGateReader struct {
	prefix       []byte
	suffix       []byte
	prefixOffset int
	suffixOffset int
	prefixReady  chan struct{}
	continueRun  chan struct{}
	suffixReady  chan struct{}
	eofRelease   chan struct{}
	prefixOnce   sync.Once
	suffixOnce   sync.Once
	eofOnce      sync.Once
}

func (r *toolArgumentGateReader) Read(p []byte) (int, error) {
	if r.prefixOffset < len(r.prefix) {
		n := copy(p, r.prefix[r.prefixOffset:])
		r.prefixOffset += n
		return n, nil
	}
	r.prefixOnce.Do(func() { close(r.prefixReady) })
	<-r.continueRun
	if r.suffixOffset < len(r.suffix) {
		n := copy(p, r.suffix[r.suffixOffset:])
		r.suffixOffset += n
		return n, nil
	}
	r.suffixOnce.Do(func() { close(r.suffixReady) })
	<-r.eofRelease
	return 0, io.EOF
}

func (r *toolArgumentGateReader) allowEOF() {
	r.eofOnce.Do(func() {
		select {
		case <-r.continueRun:
		default:
			close(r.continueRun)
		}
		select {
		case <-r.eofRelease:
		default:
			close(r.eofRelease)
		}
	})
}

func toolArgumentDeltaEvents(body string) []streamEvent {
	var events []streamEvent
	for _, event := range parseStreamEvents(body) {
		delta, _ := event.data["delta"].(map[string]interface{})
		if event.name == "content_block_delta" && delta["type"] == "input_json_delta" {
			events = append(events, event)
		}
	}
	return events
}

func toolArgumentFragments(events []streamEvent) []string {
	fragments := make([]string, 0, len(events))
	for _, event := range events {
		delta := event.data["delta"].(map[string]interface{})
		fragment, _ := delta["partial_json"].(string)
		fragments = append(fragments, fragment)
	}
	return fragments
}

func toolArgumentIndices(events []streamEvent) []float64 {
	indices := make([]float64, 0, len(events))
	for _, event := range events {
		index, _ := event.data["index"].(float64)
		indices = append(indices, index)
	}
	return indices
}

func repeatedIndex(events []streamEvent, index float64) []float64 {
	indices := make([]float64, len(events))
	for i := range indices {
		indices[i] = index
	}
	return indices
}

func TestStreamOpenAIToClaudeChoiceTypes(t *testing.T) {
	tests := []struct {
		name       string
		choices    string
		afterChunk string
		wantError  bool
	}{
		{name: "null", choices: "null", afterChunk: `data: {"choices":[{"delta":{"content":"unexpected"}}]}` + "\n\ndata: [DONE]\n\n", wantError: true},
		{name: "number", choices: "1", afterChunk: `data: {"choices":[{"delta":{"content":"unexpected"}}]}` + "\n\ndata: [DONE]\n\n", wantError: true},
		{name: "string", choices: `"x"`, afterChunk: `data: {"choices":[{"delta":{"content":"unexpected"}}]}` + "\n\ndata: [DONE]\n\n", wantError: true},
		{
			name:       "empty choices",
			choices:    "",
			afterChunk: `data: {"choices":[{"delta":{"content":"later text"}}]}` + "\n\ndata: [DONE]\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := `data: {"choices":[` + tt.choices + "]}\n\n" + tt.afterChunk
			var output bytes.Buffer
			streamOpenAIToClaude(bufio.NewWriter(&output), strings.NewReader(input), "test-model", &config.Config{}, time.Now())

			body := output.String()
			if tt.wantError {
				errorIndex := strings.Index(body, "event: error\n")
				if errorIndex < 0 {
					t.Fatalf("body %q: want an error event", body)
				}
				if strings.Contains(body[errorIndex:], "event: message_delta\n") || strings.Contains(body[errorIndex:], "event: message_stop\n") || strings.Contains(body[errorIndex+len("event: error\n"):], "event: ") {
					t.Errorf("body %q: events followed the error", body)
				}
				return
			}

			if strings.Contains(body, "event: error\n") || !strings.Contains(body, `"text":"later text"`) || !strings.Contains(body, "event: message_stop\n") {
				t.Errorf("body %q: empty choices should be skipped before the later text completes", body)
			}
		})
	}
}

func TestStreamOpenAIToClaudeDonePayload(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		want       []string
		wantAbsent []string
	}{
		{
			name: "marker text does not stop later chunks",
			input: `data: {"choices":[{"delta":{"content":"print [DONE] when finished"}}]}` + "\n" +
				`data:{"choices":[{"delta":{"content":"later text"}}]}` + "\n",
			want: []string{`"text":"print [DONE] when finished"`, `"text":"later text"`},
		},
		{
			name: "exact sentinel stops subsequent data",
			input: `data: {"choices":[{"delta":{"content":"before sentinel"}}]}` + "\n" +
				"data: [DONE]\n" +
				`data: {"choices":[{"delta":{"content":"after sentinel"}}]}` + "\n",
			want:       []string{`"text":"before sentinel"`},
			wantAbsent: []string{`"text":"after sentinel"`},
		},
		{
			name: "trimmed exact sentinel stops subsequent data",
			input: `data: {"choices":[{"delta":{"content":"before whitespace sentinel"}}]}` + "\n" +
				"data:   [DONE] \t\n" +
				`data: {"choices":[{"delta":{"content":"after whitespace sentinel"}}]}` + "\n",
			want:       []string{`"text":"before whitespace sentinel"`},
			wantAbsent: []string{`"text":"after whitespace sentinel"`},
		},
		{
			name: "non-data marker lines are ignored",
			input: ": [DONE]\n" +
				"event: [DONE]\n" +
				"id: [DONE]\n" +
				`data:{"choices":[{"delta":{"content":"data after non-data markers"}}]}` + "\n",
			want: []string{`"text":"data after non-data markers"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			streamOpenAIToClaude(bufio.NewWriter(&output), strings.NewReader(tt.input), "test-model", &config.Config{}, time.Now())

			body := output.String()
			for _, want := range tt.want {
				if !strings.Contains(body, want) {
					t.Errorf("body %q: want %q", body, want)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(body, absent) {
					t.Errorf("body %q: did not want %q", body, absent)
				}
			}
		})
	}
}
