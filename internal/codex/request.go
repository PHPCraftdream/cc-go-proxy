// Package codex converts Anthropic Messages requests/streams to and from the
// OpenAI Responses API as served by the ChatGPT Codex backend.
package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/identity"
)

// SignaturePrefix marks thinking-block signatures that carry Responses
// reasoning encrypted_content, so reasoning round-trips across turns.
const SignaturePrefix = "ccgp1:"

// Anthropic request (only fields we translate).
type anthropicRequest struct {
	Model      string           `json:"model"`
	Messages   []anthropicMsg   `json:"messages"`
	System     json.RawMessage  `json:"system"`
	Tools      []anthropicTool  `json:"tools"`
	ToolChoice *anthropicChoice `json:"tool_choice"`
	Stream     *bool            `json:"stream"`
	Thinking   *anthropicThink  `json:"thinking"`
	Metadata   map[string]any   `json:"metadata"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
}

type anthropicTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type anthropicThink struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

// Request is the Responses API body sent to the Codex backend.
type Request struct {
	Model             string     `json:"model"`
	Stream            bool       `json:"stream"`
	Input             []any      `json:"input"`
	Tools             []Tool     `json:"tools,omitempty"`
	ToolChoice        any        `json:"tool_choice"`
	ParallelToolCalls bool       `json:"parallel_tool_calls"`
	Reasoning         *Reasoning `json:"reasoning,omitempty"`
	Store             bool       `json:"store"`
	Include           []string   `json:"include"`
	PromptCacheKey    string     `json:"prompt_cache_key,omitempty"`
}

type Tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type Reasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary"`
}

type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type messageItem struct {
	Type    string        `json:"type"`
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type functionCallItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type functionOutputItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type summaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type reasoningItem struct {
	Type             string        `json:"type"`
	Summary          []summaryPart `json:"summary"`
	EncryptedContent string        `json:"encrypted_content"`
}

// ParsedRequest is an owned, decoded Anthropic Messages request. It can be
// converted to multiple backend models without decoding the body again.
type ParsedRequest struct {
	request anthropicRequest
}

// ParseRequest decodes an Anthropic Messages body. All decoded data is owned
// by the returned value and remains valid if body is subsequently changed.
func ParseRequest(body []byte) (*ParsedRequest, error) {
	var in anthropicRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}
	return &ParsedRequest{request: in}, nil
}

// Model returns the model named in the original Anthropic request.
func (p *ParsedRequest) Model() string { return p.request.Model }

// Convert translates this parsed request. model is the mapped backend model.
// wantStream reports what the client asked for; the backend call itself always
// streams.
func (p *ParsedRequest) Convert(model, cacheScope string) (req *Request, wantStream bool, err error) {
	in := &p.request
	out := &Request{
		Model:             model,
		Stream:            true,
		ToolChoice:        "auto",
		ParallelToolCalls: true,
		Store:             false,
		Include:           []string{"reasoning.encrypted_content"},
		Reasoning:         &Reasoning{Effort: effort(in.Thinking), Summary: "auto"},
	}

	if sys := identity.Rewrite(systemText(in.System), in.Model, model); sys != "" {
		out.Input = append(out.Input, messageItem{
			Type: "message", Role: "developer",
			Content: []contentPart{{Type: "input_text", Text: sys}},
		})
	}
	for i, m := range in.Messages {
		items, err := convertMessage(m, in.Model, model)
		if err != nil {
			return nil, false, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out.Input = append(out.Input, items...)
	}

	for _, t := range in.Tools {
		// Server tools (web_search_*, bash_*, ...) carry no schema: not translatable.
		if len(t.InputSchema) == 0 || (t.Type != "" && t.Type != "custom") {
			continue
		}
		out.Tools = append(out.Tools, Tool{
			Type: "function", Name: t.Name, Description: t.Description,
			Parameters: t.InputSchema, Strict: false,
		})
	}
	out.ToolChoice = toolChoice(in.ToolChoice)
	if len(out.Tools) == 0 {
		out.ToolChoice = "auto"
	}

	// Stable per-session key improves backend prompt caching.
	if uid, _ := in.Metadata["user_id"].(string); uid != "" {
		h := sha256.Sum256([]byte(cacheScope + "\x00" + uid))
		out.PromptCacheKey = hex.EncodeToString(h[:16])
	}
	return out, in.Stream != nil && *in.Stream, nil
}

// ConvertRequest translates an Anthropic Messages body. model is the mapped
// backend model. wantStream reports what the client asked for; the backend
// call itself always streams.
func ConvertRequest(body []byte, model, cacheScope string) (*Request, bool, error) {
	parsed, err := ParseRequest(body)
	if err != nil {
		return nil, false, err
	}
	return parsed.Convert(model, cacheScope)
}

func effort(t *anthropicThink) string {
	if t == nil || t.Type != "enabled" {
		return "medium"
	}
	switch {
	case t.BudgetTokens <= 4096:
		return "low"
	case t.BudgetTokens <= 16384:
		return "medium"
	default:
		return "high"
	}
}

func toolChoice(c *anthropicChoice) any {
	if c == nil {
		return "auto"
	}
	switch c.Type {
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return map[string]string{"type": "function", "name": c.Name}
	}
	return "auto"
}

// systemText joins system blocks, dropping Claude Code's billing header block.
func systemText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	if trimmed[0] == 'n' {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) != nil {
			return ""
		}
		if strings.HasPrefix(s, "x-anthropic-billing-header") {
			if newline := strings.IndexByte(s, '\n'); newline >= 0 {
				return s[newline+1:]
			}
			return ""
		}
		return s
	}
	if trimmed[0] != '[' {
		return ""
	}
	var blocks []anthropicBlock
	if json.Unmarshal(trimmed, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" && !strings.HasPrefix(b.Text, "x-anthropic-billing-header") {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func parseBlocks(raw json.RawMessage) ([]anthropicBlock, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 {
		switch trimmed[0] {
		case '"':
			var s string
			if err := json.Unmarshal(trimmed, &s); err != nil {
				return nil, fmt.Errorf("content: %w", err)
			}
			return []anthropicBlock{{Type: "text", Text: s}}, nil
		case '[':
			var blocks []anthropicBlock
			if err := json.Unmarshal(trimmed, &blocks); err != nil {
				return nil, fmt.Errorf("content: %w", err)
			}
			return blocks, nil
		case 'n':
			if bytes.Equal(trimmed, []byte("null")) {
				// Unmarshalling null into a string historically succeeds as empty text.
				return []anthropicBlock{{Type: "text"}}, nil
			}
		}
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}
	return blocks, nil
}

func convertMessage(m anthropicMsg, requested, backend string) ([]any, error) {
	blocks, err := parseBlocks(m.Content)
	if err != nil {
		return nil, err
	}
	if m.Role != "assistant" && identity.Mentions(m.Content) {
		for i := range blocks {
			if blocks[i].Type == "text" {
				blocks[i].Text = identity.Rewrite(blocks[i].Text, requested, backend)
			}
		}
	}
	if m.Role == "assistant" {
		return convertAssistant(blocks), nil
	}
	return convertUser(blocks), nil
}

// convertUser: tool results first (they answer the preceding calls), then content.
func convertUser(blocks []anthropicBlock) []any {
	var items []any
	var parts []contentPart
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			items = append(items, functionOutputItem{
				Type: "function_call_output", CallID: b.ToolUseID, Output: toolResultText(b),
			})
		case "text":
			if b.Text != "" {
				parts = append(parts, contentPart{Type: "input_text", Text: b.Text})
			}
		case "image":
			if u := imageURL(b); u != "" {
				parts = append(parts, contentPart{Type: "input_image", ImageURL: u})
			}
		case "document":
			parts = append(parts, contentPart{Type: "input_text", Text: "[document omitted: not supported by ccgp]"})
		}
	}
	if len(parts) > 0 {
		items = append(items, messageItem{Type: "message", Role: "user", Content: parts})
	}
	return items
}

func convertAssistant(blocks []anthropicBlock) []any {
	var items []any
	var text []contentPart
	flush := func() {
		if len(text) > 0 {
			items = append(items, messageItem{Type: "message", Role: "assistant", Content: text})
			text = nil
		}
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				text = append(text, contentPart{Type: "output_text", Text: b.Text})
			}
		case "tool_use":
			flush()
			args := string(b.Input)
			if args == "" || args == "null" {
				args = "{}"
			}
			items = append(items, functionCallItem{
				Type: "function_call", CallID: b.ID, Name: b.Name, Arguments: args,
			})
		case "thinking":
			// Only our own signatures carry backend reasoning state.
			if enc, ok := strings.CutPrefix(b.Signature, SignaturePrefix); ok && enc != "" {
				flush()
				summary := []summaryPart{}
				if b.Thinking != "" {
					summary = append(summary, summaryPart{Type: "summary_text", Text: b.Thinking})
				}
				items = append(items, reasoningItem{Type: "reasoning", Summary: summary, EncryptedContent: enc})
			}
		}
	}
	flush()
	return items
}

func toolResultText(b anthropicBlock) string {
	var text string
	if len(b.Content) > 0 {
		if blocks, err := parseBlocks(b.Content); err == nil {
			var parts []string
			for _, c := range blocks {
				switch c.Type {
				case "text":
					parts = append(parts, c.Text)
				case "image":
					parts = append(parts, "[image omitted]")
				}
			}
			text = strings.Join(parts, "\n")
		}
	}
	if b.IsError {
		return "[tool error] " + text
	}
	return text
}

func imageURL(b anthropicBlock) string {
	if b.Source == nil {
		return ""
	}
	switch b.Source.Type {
	case "base64":
		return "data:" + b.Source.MediaType + ";base64," + b.Source.Data
	case "url":
		return b.Source.URL
	}
	return ""
}
