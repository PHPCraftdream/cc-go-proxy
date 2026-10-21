package sessions

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

// linkKeys hold the uuid of another transcript line.
var linkKeys = []string{"parentUuid", "logicalParentUuid", "leafUuid"}

// thinkingMark appears in the raw text of every line that has a thinking or
// redacted_thinking block, so other lines need no JSON parsing.
var thinkingMark = []byte(`thinking"`)

// StripThinking is StripThinkingStream on a byte slice.
func StripThinking(data []byte) ([]byte, int, error) {
	var out bytes.Buffer
	out.Grow(len(data))
	n, err := StripThinkingStream(bytes.NewReader(data), &out)
	if err != nil {
		return nil, 0, err
	}
	return out.Bytes(), n, nil
}

// StripThinkingStream copies a transcript from r to w without its thinking and
// redacted_thinking blocks. Their signatures are bound to the provider that
// produced them, so another provider (the official API in particular) would
// reject them.
//
// Claude Code writes one line per content block, chained by parentUuid. A line
// left without content is dropped and its children are re-attached to its
// parent, so the conversation stays connected. Lines that need no change are
// copied byte for byte. It returns the number of blocks removed; errors from w
// are returned as they are.
func StripThinkingStream(r io.Reader, w io.Writer) (int, error) {
	br := bufio.NewReaderSize(r, copyBuf)
	redirect := map[string]json.RawMessage{} // dropped line's uuid -> parentUuid to use instead
	removed := 0
	for {
		seg, rerr := br.ReadBytes('\n')
		if len(seg) > 0 {
			line := bytes.TrimRight(seg, "\r\n")
			eol := seg[len(line):]
			rewritten, drop, n, err := stripLine(line, redirect)
			if err != nil {
				return 0, err
			}
			removed += n
			switch {
			case drop:
			case rewritten != nil:
				if _, err := w.Write(append(rewritten, eol...)); err != nil {
					return 0, err
				}
			default:
				if _, err := w.Write(seg); err != nil {
					return 0, err
				}
			}
		}
		if rerr == io.EOF {
			return removed, nil
		}
		if rerr != nil {
			return 0, rerr
		}
	}
}

// stripLine returns the replacement for line, nil when it stays as is, or
// drop=true when it must disappear.
func stripLine(line []byte, redirect map[string]json.RawMessage) (rewritten []byte, drop bool, removed int, err error) {
	if len(line) == 0 || line[0] != '{' {
		return nil, false, 0, nil
	}
	if !bytes.Contains(line, thinkingMark) && !linksTo(line, redirect) {
		return nil, false, 0, nil
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(line, &top) != nil {
		return nil, false, 0, nil // not ours to judge
	}

	changed := false
	for _, k := range linkKeys {
		var id string
		if raw, ok := top[k]; ok && json.Unmarshal(raw, &id) == nil {
			if to, ok := redirect[id]; ok {
				top[k] = to
				changed = true
			}
		}
	}

	if rawMsg, ok := top["message"]; ok {
		var msg map[string]json.RawMessage
		var blocks []json.RawMessage
		if json.Unmarshal(rawMsg, &msg) == nil && json.Unmarshal(msg["content"], &blocks) == nil && len(blocks) > 0 {
			kept := make([]json.RawMessage, 0, len(blocks))
			for _, b := range blocks {
				if isThinking(b) {
					removed++
				} else {
					kept = append(kept, b)
				}
			}
			if removed > 0 {
				if len(kept) == 0 {
					var id string
					if raw, ok := top["uuid"]; ok && json.Unmarshal(raw, &id) == nil {
						parent := top["parentUuid"]
						if parent == nil {
							parent = json.RawMessage("null")
						}
						redirect[id] = parent
					}
					return nil, true, removed, nil
				}
				if msg["content"], err = encode(kept); err != nil {
					return nil, false, 0, err
				}
				if top["message"], err = encode(msg); err != nil {
					return nil, false, 0, err
				}
				changed = true
			}
		}
	}

	if !changed {
		return nil, false, 0, nil
	}
	rewritten, err = encode(top)
	return rewritten, false, removed, err
}

// linksTo reports whether line may refer to a dropped line through one of the
// link keys. It over-approximates (a hit only costs a full parse) but never
// misses a key whose value is a plain string.
func linksTo(line []byte, redirect map[string]json.RawMessage) bool {
	if len(redirect) == 0 {
		return false
	}
	for _, k := range linkKeys {
		mark := []byte(`"` + k + `"`)
		for rest := line; ; {
			i := bytes.Index(rest, mark)
			if i < 0 {
				break
			}
			rest = rest[i+len(mark):]
			v := bytes.TrimLeft(rest, " \t")
			if len(v) == 0 || v[0] != ':' {
				continue
			}
			v = bytes.TrimLeft(v[1:], " \t")
			if len(v) == 0 || v[0] != '"' {
				continue
			}
			v = v[1:]
			end := bytes.IndexByte(v, '"')
			if end < 0 {
				return true // malformed: let the parser decide
			}
			if _, ok := redirect[string(v[:end])]; ok {
				return true
			}
			if bytes.IndexByte(v[:end], '\\') >= 0 {
				return true // escaped value: not a plain uuid
			}
		}
	}
	return false
}

func isThinking(block json.RawMessage) bool {
	var b struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(block, &b) == nil && (b.Type == "thinking" || b.Type == "redacted_thinking")
}

// encode marshals without HTML escaping, which would alter the text.
func encode(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
