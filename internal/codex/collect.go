package codex

import (
	"encoding/json"
	"errors"
	"strings"
)

type blockStrings struct {
	text, thinking *strings.Builder
}

// Collector assembles Translator events into a non-streaming Messages response.
type Collector struct {
	msg          map[string]any
	blocks       []map[string]any
	textThinking []blockStrings
	json         map[int]*strings.Builder
	err          error
}

func seededBuilder(initial string) *strings.Builder {
	b := &strings.Builder{}
	b.WriteString(initial)
	return b
}

func (c *Collector) materialize(i int) {
	if i < 0 || i >= len(c.blocks) {
		return
	}
	if b := c.textThinking[i].text; b != nil {
		c.blocks[i]["text"] = b.String()
	}
	if b := c.textThinking[i].thinking; b != nil {
		c.blocks[i]["thinking"] = b.String()
	}
}

func (c *Collector) Emit(event string, data any) {
	d, _ := data.(map[string]any)
	switch event {
	case "message_start":
		c.msg, _ = d["message"].(map[string]any)
	case "content_block_start":
		cb, _ := d["content_block"].(map[string]any)
		cp := map[string]any{}
		for k, v := range cb {
			cp[k] = v
		}
		c.blocks = append(c.blocks, cp)
		c.textThinking = append(c.textThinking, blockStrings{})
	case "content_block_delta":
		i, _ := d["index"].(int)
		if i < 0 || i >= len(c.blocks) {
			return
		}
		b, delta := c.blocks[i], d["delta"].(map[string]any)
		switch delta["type"] {
		case "text_delta":
			if c.textThinking[i].text == nil {
				c.textThinking[i].text = seededBuilder(b["text"].(string))
			}
			c.textThinking[i].text.WriteString(delta["text"].(string))
		case "thinking_delta":
			if c.textThinking[i].thinking == nil {
				c.textThinking[i].thinking = seededBuilder(b["thinking"].(string))
			}
			c.textThinking[i].thinking.WriteString(delta["thinking"].(string))
		case "signature_delta":
			b["signature"] = delta["signature"]
		case "input_json_delta":
			if c.json == nil {
				c.json = map[int]*strings.Builder{}
			}
			if c.json[i] == nil {
				c.json[i] = &strings.Builder{}
			}
			c.json[i].WriteString(delta["partial_json"].(string))
		}
	case "content_block_stop":
		i, _ := d["index"].(int)
		c.materialize(i)
		if sb := c.json[i]; sb != nil && i < len(c.blocks) {
			var input any
			if json.Unmarshal([]byte(sb.String()), &input) == nil {
				c.blocks[i]["input"] = input
			}
		}
	case "message_delta":
		if c.msg == nil {
			return
		}
		if delta, ok := d["delta"].(map[string]any); ok {
			c.msg["stop_reason"] = delta["stop_reason"]
		}
		c.msg["usage"] = d["usage"]
	case "error":
		e, _ := d["error"].(map[string]any)
		msg, _ := e["message"].(string)
		c.err = errors.New(msg)
	}
}

// Result returns the assembled response or the stream error.
func (c *Collector) Result() (map[string]any, error) {
	if c.err != nil {
		return nil, c.err
	}
	if c.msg == nil {
		return nil, errors.New("codex: empty response")
	}
	content := make([]any, len(c.blocks))
	for i, b := range c.blocks {
		c.materialize(i)
		content[i] = b
	}
	c.msg["content"] = content
	return c.msg, nil
}
