package codex

import (
	"encoding/json"
	"fmt"
)

// WSChain is what the backend holds after a completed response on one websocket
// connection: the full input it was sent (as hashes) and the output it produced.
type WSChain struct {
	responseID  string
	fingerprint Fingerprint
	inputLen    int // items of the full input the backend holds (hashes cover their content)
	hashes      []uint64
	output      []json.RawMessage
}

// NewChain records req (the full request just sent) and the output items of its
// completed response; nil when there is no cache key to chain by.
func NewChain(req *Request, responseID string, outputItems []json.RawMessage) *WSChain {
	if req == nil || req.PromptCacheKey == "" {
		return nil
	}
	chain := &WSChain{
		responseID:  responseID,
		fingerprint: NewFingerprint(req),
		inputLen:    len(req.Input),
		hashes:      append([]uint64(nil), ItemHashes(req)...),
		output:      make([]json.RawMessage, len(outputItems)),
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
	if next == nil || next.PromptCacheKey == "" {
		return nil, false, "no cache key"
	}
	if c == nil {
		return nil, false, "no chain"
	}
	if c.responseID == "" {
		return nil, false, "no response id"
	}
	current := NewFingerprint(next)
	if current.Key != c.fingerprint.Key {
		return nil, false, "cache key changed"
	}
	if current.Model != c.fingerprint.Model {
		return nil, false, "model changed"
	}
	if current.Tools != c.fingerprint.Tools || current.NTools != c.fingerprint.NTools {
		return nil, false, "tools changed"
	}
	if current.System != c.fingerprint.System {
		return nil, false, "system changed"
	}
	if current.Reasoning != c.fingerprint.Reasoning {
		return nil, false, "reasoning changed"
	}
	if current.ToolChoice != c.fingerprint.ToolChoice {
		return nil, false, "tool choice changed"
	}
	hashes := ItemHashes(next)
	for i := 0; i < min(len(hashes), len(c.hashes)); i++ {
		if hashes[i] != c.hashes[i] {
			return nil, false, fmt.Sprintf("history diverged at item %d", i)
		}
	}
	if len(hashes) < len(c.hashes) {
		return nil, false, fmt.Sprintf("history diverged at item %d", len(hashes))
	}
	if len(next.Input) < c.inputLen {
		return nil, false, fmt.Sprintf("history diverged at item %d", len(next.Input))
	}
	cursor, matched := c.matchOutput(next.Input, c.inputLen)
	if !matched {
		return nil, false, "output mismatch"
	}
	if cursor == len(next.Input) {
		return nil, false, "nothing new"
	}
	for _, item := range next.Input[cursor:] {
		if !validDeltaItem(item) {
			return nil, false, "output mismatch"
		}
	}
	delta, err := cloneAnySlice(next.Input[cursor:])
	if err != nil {
		return nil, false, "output mismatch"
	}
	return delta, true, ""
}

func (c *WSChain) matchOutput(input []any, cursor int) (int, bool) {
	for _, raw := range c.output {
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &head) != nil {
			return cursor, false
		}
		switch head.Type {
		case "message":
			var output struct {
				Type string `json:"type"`
				Role string `json:"role"`
			}
			if json.Unmarshal(raw, &output) != nil || output.Type != "message" || output.Role != "assistant" || cursor >= len(input) {
				return cursor, false
			}
			var converted struct {
				Type string `json:"type"`
				Role string `json:"role"`
			}
			if !decodeInputItem(input[cursor], &converted) || converted.Type != "message" || converted.Role != "assistant" {
				return cursor, false
			}
			cursor++
		case "reasoning":
			var output reasoningItem
			if json.Unmarshal(raw, &output) != nil || output.Type != "reasoning" {
				return cursor, false
			}
			if cursor < len(input) && inputKind(input[cursor]) == "reasoning" {
				var converted reasoningItem
				if !decodeInputItem(input[cursor], &converted) || output.EncryptedContent == "" || converted.EncryptedContent != output.EncryptedContent {
					return cursor, false
				}
				cursor++
			}
		case "function_call":
			var output struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
			}
			if json.Unmarshal(raw, &output) != nil || output.Type != "function_call" || output.CallID == "" || cursor >= len(input) {
				return cursor, false
			}
			var converted struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
			}
			if !decodeInputItem(input[cursor], &converted) || converted.Type != "function_call" || converted.CallID != output.CallID {
				return cursor, false
			}
			cursor++
		default:
			return cursor, false
		}
	}
	return cursor, true
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

func inputKind(item any) string {
	b, err := json.Marshal(item)
	if err != nil {
		return ""
	}
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(b, &head) != nil {
		return ""
	}
	return head.Type
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
