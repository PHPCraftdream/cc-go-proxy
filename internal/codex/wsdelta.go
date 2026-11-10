package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// WSChain is what the backend holds after a completed response on one websocket
// connection: the full input it was sent (as hashes) and the output it produced.
type WSChain struct {
	responseID  string
	fingerprint Fingerprint
	inputLen    int // items of the full input the backend holds (hashes cover their content)
	hashes      []uint64
	output      []json.RawMessage
	// For the log when a later request diverges: the kind and size of every
	// input item, the encoding of the last one, when the response completed.
	shapes []ItemShape
	last   []byte
	done   time.Time
}

// NewChain records req (the full request just sent) and the output items of its
// completed response; nil when there is no cache key to chain by.
func NewChain(req *Request, responseID string, outputItems []json.RawMessage) *WSChain {
	if req == nil || req.PromptCacheKey == "" {
		return nil
	}
	hashes, sizes, last := itemDigests(req, true)
	chain := &WSChain{
		responseID:  responseID,
		fingerprint: NewFingerprint(req),
		inputLen:    len(req.Input),
		hashes:      hashes,
		output:      make([]json.RawMessage, len(outputItems)),
		shapes:      make([]ItemShape, len(req.Input)),
		last:        last,
	}
	for i, item := range req.Input {
		chain.shapes[i] = itemShape(item)
		chain.shapes[i].Size = sizes[i]
	}
	for i := range outputItems {
		chain.output[i] = append(json.RawMessage(nil), outputItems[i]...)
	}
	return chain
}

// ResponseID is the id to send as previous_response_id.
func (c *WSChain) ResponseID() string {
	if c == nil {
		return ""
	}
	return c.responseID
}

// Delta returns the items of next that the backend does not hold yet, or
// ok=false and a short reason when next must be sent in full.
func (c *WSChain) Delta(next *Request) (items []any, ok bool, reason string) {
	items, ok, reason, _ = c.delta(next)
	return items, ok, reason
}

// delta is Delta plus, when the history diverged, a description of where.
func (c *WSChain) delta(next *Request) (items []any, ok bool, reason string, div *WSDivergence) {
	if next == nil || next.PromptCacheKey == "" {
		return nil, false, "no cache key", nil
	}
	if c == nil {
		return nil, false, "no chain", nil
	}
	if c.responseID == "" {
		return nil, false, "no response id", nil
	}
	current := NewFingerprint(next)
	if current.Key != c.fingerprint.Key {
		return nil, false, "cache key changed", nil
	}
	if current.Model != c.fingerprint.Model {
		return nil, false, "model changed", nil
	}
	if current.Tools != c.fingerprint.Tools || current.NTools != c.fingerprint.NTools {
		return nil, false, "tools changed", nil
	}
	if current.System != c.fingerprint.System {
		return nil, false, "system changed", nil
	}
	if current.Reasoning != c.fingerprint.Reasoning {
		return nil, false, "reasoning changed", nil
	}
	if current.ToolChoice != c.fingerprint.ToolChoice {
		return nil, false, "tool choice changed", nil
	}
	hashes := ItemHashes(next)
	diverged := func(at int) ([]any, bool, string, *WSDivergence) {
		return nil, false, fmt.Sprintf("history diverged at item %d of %d", at, len(c.hashes)), c.divergence(next, at)
	}
	for i := 0; i < min(len(hashes), len(c.hashes)); i++ {
		if hashes[i] != c.hashes[i] {
			return diverged(i)
		}
	}
	if len(hashes) < len(c.hashes) {
		return diverged(len(hashes))
	}
	if len(next.Input) < c.inputLen {
		return diverged(len(next.Input))
	}
	cursor, why := c.matchOutput(next.Input, c.inputLen)
	if why != "" {
		return nil, false, why, nil
	}
	if cursor == len(next.Input) {
		return nil, false, "nothing new", nil
	}
	for _, item := range next.Input[cursor:] {
		if !validDeltaItem(item) {
			return nil, false, "new item not sendable (" + itemShape(item).Kind + ")", nil
		}
	}
	delta, err := cloneAnySlice(next.Input[cursor:])
	if err != nil {
		return nil, false, "delta copy failed", nil
	}
	return delta, true, "", nil
}

// matchOutput checks that input, from cursor on, holds exactly the output the
// backend produced, by content: the same assistant text, the same tool calls
// (id, name, arguments as JSON values) and the same reasoning (encrypted
// content). Anything else would let the backend continue from a context the
// client no longer has, so it returns the reason to send the request in full.
func (c *WSChain) matchOutput(input []any, cursor int) (int, string) {
	for _, raw := range c.output {
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &head) != nil {
			return cursor, "output unreadable"
		}
		if cursor >= len(input) {
			return cursor, "output missing"
		}
		switch head.Type {
		case "message":
			var output, converted wsMessage
			if json.Unmarshal(raw, &output) != nil || output.Role != "assistant" {
				return cursor, "output not an assistant message"
			}
			if !decodeInputItem(input[cursor], &converted) || converted.Type != "message" || converted.Role != "assistant" {
				return cursor, "assistant message missing (found " + itemShape(input[cursor]).Kind + ")"
			}
			if output.text() != converted.text() {
				return cursor, "assistant text changed"
			}
		case "reasoning":
			var output, converted reasoningItem
			if json.Unmarshal(raw, &output) != nil || output.EncryptedContent == "" {
				return cursor, "output reasoning without content"
			}
			if !decodeInputItem(input[cursor], &converted) || converted.Type != "reasoning" {
				return cursor, "reasoning dropped"
			}
			if converted.EncryptedContent != output.EncryptedContent {
				return cursor, "reasoning differs"
			}
		case "function_call":
			var output, converted functionCallItem
			if json.Unmarshal(raw, &output) != nil || output.CallID == "" {
				return cursor, "output tool call without id"
			}
			if !decodeInputItem(input[cursor], &converted) || converted.Type != "function_call" {
				return cursor, "tool call missing (found " + itemShape(input[cursor]).Kind + ")"
			}
			if field := toolCallDifference(converted, output); field != "" {
				return cursor, "tool call changed (" + field + ", " + safeToolName(output.Name) + ")"
			}
		default:
			return cursor, "output item not compared (" + safeItemType(head.Type) + ")"
		}
		cursor++
	}
	return cursor, ""
}

// wsMessage is a message item as the backend sends it and as the converter
// writes it back (output_text parts).
type wsMessage struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (m wsMessage) text() string {
	var b strings.Builder
	for _, part := range m.Content {
		b.WriteString(part.Text)
	}
	return b.String()
}

// sameJSON compares two JSON texts as values: key order and spacing do not
// matter (the client stores tool arguments as an object, the converter writes
// them out again). Empty and "{}" are the same (no arguments).
func sameJSON(a, b string) bool {
	if a == b {
		return true
	}
	if a == "" {
		a = "{}"
	}
	if b == "" {
		b = "{}"
	}
	var av, bv any
	if json.Unmarshal([]byte(a), &av) != nil || json.Unmarshal([]byte(b), &bv) != nil {
		return false
	}
	ab, errA := json.Marshal(av)
	bb, errB := json.Marshal(bv)
	return errA == nil && errB == nil && string(ab) == string(bb)
}

func decodeInputItem(item any, target any) bool {
	b, err := json.Marshal(item)
	return err == nil && json.Unmarshal(b, target) == nil
}

func validDeltaItem(item any) bool {
	b, err := json.Marshal(item)
	if err != nil {
		return false
	}
	var value struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if json.Unmarshal(b, &value) != nil {
		return false
	}
	switch value.Type {
	case "function_call_output":
		return true
	case "message":
		return value.Role == "user"
	default:
		return false
	}
}

func cloneAnySlice(input []any) ([]any, error) {
	if input == nil {
		return nil, nil
	}
	b, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var cloned []any
	if err := json.Unmarshal(b, &cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}

// WSCreatePayload is the response.create frame for req with input as its items,
// continuing previousResponseID when it is not empty.
func WSCreatePayload(req *Request, previousResponseID string, input []any) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}
	payload := struct {
		Type               string     `json:"type"`
		Model              string     `json:"model"`
		Stream             bool       `json:"stream"`
		Input              []any      `json:"input"`
		Tools              []Tool     `json:"tools,omitempty"`
		ToolChoice         any        `json:"tool_choice"`
		ParallelToolCalls  bool       `json:"parallel_tool_calls"`
		Reasoning          *Reasoning `json:"reasoning,omitempty"`
		Store              bool       `json:"store"`
		Include            []string   `json:"include"`
		PromptCacheKey     string     `json:"prompt_cache_key,omitempty"`
		PreviousResponseID string     `json:"previous_response_id,omitempty"`
	}{
		Type:               "response.create",
		Model:              req.Model,
		Stream:             true,
		Input:              input,
		Tools:              req.Tools,
		ToolChoice:         req.ToolChoice,
		ParallelToolCalls:  req.ParallelToolCalls,
		Reasoning:          req.Reasoning,
		Store:              req.Store,
		Include:            req.Include,
		PromptCacheKey:     req.PromptCacheKey,
		PreviousResponseID: previousResponseID,
	}
	return json.Marshal(payload)
}

// toolCallDifference names the first field in which the client's tool call
// differs from the backend's: id, name or arguments; "" when they are the same.
func toolCallDifference(got, want functionCallItem) string {
	switch {
	case got.CallID != want.CallID:
		return "id"
	case got.Name != want.Name:
		return "name"
	case !sameJSON(got.Arguments, want.Arguments):
		return "arguments"
	}
	return ""
}

// safeToolName is a tool name fit for a log line: letters, digits and _.:- only,
// at most 40 characters, "?" for anything else.
func safeToolName(name string) string {
	if name == "" || len(name) > 40 {
		return "?"
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == ':', r == '-':
		default:
			return "?"
		}
	}
	return name
}
