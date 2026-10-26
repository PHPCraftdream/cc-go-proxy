package codex

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ErrRetry ends Run when the backend failed in a way worth one more try
// (overloaded) before any content was sent; see Translator.AllowRetry.
var ErrRetry = errors.New("codex: backend failed before output, retry")

// Emit receives one Anthropic SSE event (name + payload).
type Emit func(event string, data any)

// event is the subset of Responses stream events we consume.
type event struct {
	Type         string `json:"type"`
	ItemID       string `json:"item_id"`
	Delta        string `json:"delta"`
	SummaryIndex int    `json:"summary_index"`
	Item         *struct {
		ID               string `json:"id"`
		Type             string `json:"type"`
		CallID           string `json:"call_id"`
		Name             string `json:"name"`
		Arguments        string `json:"arguments"`
		EncryptedContent string `json:"encrypted_content"`
	} `json:"item"`
	Response *struct {
		ID    string `json:"id"`
		Usage *struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
	// top-level `error` events: code/message at the top or in an `error` object
	Code    string `json:"code"`
	Message string `json:"message"`
	Error   *struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Usage is the token count of a response.
type Usage struct {
	Input, Cached, Output, Reasoning int
}

type block struct {
	index    int
	kind     string // thinking | text | tool_use
	sawDelta bool
}

// Translator converts a Responses SSE stream into Anthropic Messages events.
type Translator struct {
	Model string
	Emit  Emit
	// Failed is the message of the error event sent to the client, if any.
	Failed string
	// Detail is the raw upstream event that failed the stream (error or
	// response.failed), for the log.
	Detail string
	// FirstDelta is when the first thinking, text or tool input arrived (zero: none).
	FirstDelta time.Time
	// Usage is the token count of the finished response (zero when none was sent).
	Usage Usage
	// AllowRetry makes an overloaded backend end Run with ErrRetry instead of
	// failing the stream, while no content block has been sent; Retry then
	// holds the reason. The caller sends the request again and runs the new
	// stream through the same Translator.
	AllowRetry bool
	Retry      string

	started bool
	next    int
	open    map[string]*block // item id -> open block
	toolUse bool
	done    bool
	retry   bool
}

// Run consumes the upstream SSE body until completion or EOF.
func (t *Translator) Run(r io.Reader) error {
	t.open = map[string]*block{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20) // encrypted reasoning can be large
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if ev.Type == "error" || ev.Type == "response.failed" {
			t.Detail = data
		}
		t.handle(&ev)
		if t.retry {
			t.retry = false
			return ErrRetry
		}
		if t.done {
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		t.fail("stream read error: " + err.Error())
		return err
	}
	if !t.done {
		t.fail("upstream stream ended before completion")
	}
	return nil
}

func (t *Translator) start(id string) {
	if t.started {
		return
	}
	t.started = true
	if id == "" {
		id = "msg_ccgp"
	}
	t.Emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": t.Model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (t *Translator) handle(ev *event) {
	switch ev.Type {
	case "response.created", "response.in_progress":
		id := ""
		if ev.Response != nil {
			id = ev.Response.ID
		}
		t.start(id)

	case "response.output_item.added":
		if ev.Item == nil {
			return
		}
		t.start("")
		switch ev.Item.Type {
		case "reasoning":
			t.openBlock(ev.Item.ID, "thinking", map[string]any{"type": "thinking", "thinking": ""})
		case "message":
			t.openBlock(ev.Item.ID, "text", map[string]any{"type": "text", "text": ""})
		case "function_call":
			t.toolUse = true
			t.openBlock(ev.Item.ID, "tool_use", map[string]any{
				"type": "tool_use", "id": ev.Item.CallID, "name": ev.Item.Name, "input": map[string]any{},
			})
		}

	case "response.reasoning_summary_part.added":
		if ev.SummaryIndex > 0 {
			t.delta(ev.ItemID, map[string]any{"type": "thinking_delta", "thinking": "\n\n"})
		}
	case "response.reasoning_summary_text.delta":
		t.delta(ev.ItemID, map[string]any{"type": "thinking_delta", "thinking": ev.Delta})
	case "response.output_text.delta":
		t.delta(ev.ItemID, map[string]any{"type": "text_delta", "text": ev.Delta})
	case "response.function_call_arguments.delta":
		t.delta(ev.ItemID, map[string]any{"type": "input_json_delta", "partial_json": ev.Delta})

	case "response.output_item.done":
		if ev.Item == nil {
			return
		}
		b := t.open[ev.Item.ID]
		if b == nil {
			return
		}
		switch b.kind {
		case "thinking":
			if ev.Item.EncryptedContent != "" {
				t.delta(ev.Item.ID, map[string]any{"type": "signature_delta", "signature": SignaturePrefix + ev.Item.EncryptedContent})
			}
		case "tool_use":
			if !b.sawDelta && ev.Item.Arguments != "" {
				t.delta(ev.Item.ID, map[string]any{"type": "input_json_delta", "partial_json": ev.Item.Arguments})
			}
		}
		t.closeBlock(ev.Item.ID)

	case "response.completed", "response.incomplete":
		t.finish(ev)

	case "response.failed":
		code, msg := "", ""
		if ev.Response != nil && ev.Response.Error != nil {
			code, msg = ev.Response.Error.Code, ev.Response.Error.Message
		}
		t.failOrRetry(overloaded(code, ""), describeError(code, msg, "response failed"))

	case "error":
		code, msg, typ := ev.Code, ev.Message, ""
		if ev.Error != nil {
			typ = ev.Error.Type
			code = cmp.Or(code, ev.Error.Code, ev.Error.Type)
			msg = cmp.Or(msg, ev.Error.Message)
		}
		t.failOrRetry(overloaded(code, typ), describeError(code, msg, "upstream error"))
	}
}

// overloaded reports whether an upstream error says the backend could not take
// the request right now, so the same request may succeed a moment later.
func overloaded(code, typ string) bool {
	switch code {
	case "server_is_overloaded", "server_error", "service_unavailable":
		return true
	}
	return typ == "service_unavailable_error" || typ == "server_error"
}

// failOrRetry fails the stream, or marks it for a retry when the failure is
// worth one, retries are allowed and no content was sent yet.
func (t *Translator) failOrRetry(worth bool, msg string) {
	if worth && t.AllowRetry && t.next == 0 && !t.done {
		t.Retry, t.retry = msg, true
		return
	}
	t.fail(msg)
}

// Started reports whether message_start has been sent.
func (t *Translator) Started() bool { return t.started }

// Fail ends the stream with an error event for the client (unless it already ended).
func (t *Translator) Fail(msg string) { t.fail(msg) }

// describeError joins an upstream error code and message; fallback names the
// failure when the message is missing.
func describeError(code, msg, fallback string) string {
	switch {
	case msg != "" && code != "":
		return code + ": " + msg
	case msg != "":
		return msg
	case code != "":
		return fallback + " " + code
	}
	return fallback
}

func (t *Translator) openBlock(itemID, kind string, cb map[string]any) {
	b := &block{index: t.next, kind: kind}
	t.next++
	t.open[itemID] = b
	t.Emit("content_block_start", map[string]any{"type": "content_block_start", "index": b.index, "content_block": cb})
}

func (t *Translator) delta(itemID string, d map[string]any) {
	b := t.open[itemID]
	if b == nil {
		return
	}
	if d["type"] == "input_json_delta" {
		b.sawDelta = true
	}
	if t.FirstDelta.IsZero() && d["type"] != "signature_delta" {
		t.FirstDelta = time.Now()
	}
	t.Emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": b.index, "delta": d})
}

func (t *Translator) closeBlock(itemID string) {
	b := t.open[itemID]
	if b == nil {
		return
	}
	delete(t.open, itemID)
	t.Emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": b.index})
}

func (t *Translator) closeAll() {
	// Close in index order for well-formed output.
	for len(t.open) > 0 {
		minID, minIdx := "", -1
		for id, b := range t.open {
			if minIdx < 0 || b.index < minIdx {
				minID, minIdx = id, b.index
			}
		}
		t.closeBlock(minID)
	}
}

func (t *Translator) finish(ev *event) {
	t.start("")
	t.closeAll()
	stop := "end_turn"
	if t.toolUse {
		stop = "tool_use"
	}
	usage := map[string]any{"output_tokens": 0}
	if r := ev.Response; r != nil {
		if r.IncompleteDetails != nil && r.IncompleteDetails.Reason == "max_output_tokens" {
			stop = "max_tokens"
		}
		if u := r.Usage; u != nil {
			cached := u.InputTokensDetails.CachedTokens
			t.Usage = Usage{Input: u.InputTokens, Cached: cached, Output: u.OutputTokens, Reasoning: u.OutputTokensDetails.ReasoningTokens}
			usage = map[string]any{
				"input_tokens":            u.InputTokens - cached,
				"cache_read_input_tokens": cached,
				"output_tokens":           u.OutputTokens,
			}
		}
	}
	t.Emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": usage,
	})
	t.Emit("message_stop", map[string]any{"type": "message_stop"})
	t.done = true
}

func (t *Translator) fail(msg string) {
	if t.done {
		return
	}
	t.done = true
	t.Failed = msg
	t.Emit("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": fmt.Sprintf("codex: %s", msg)},
	})
}
