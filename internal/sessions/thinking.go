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
	s := newThinkingStripper()
	for {
		seg, rerr := br.ReadBytes('\n')
		if len(seg) > 0 {
			out, err := s.line(seg)
			if err != nil {
				return 0, err
			}
			if len(out) > 0 {
				if _, err := w.Write(out); err != nil {
					return 0, err
				}
			}
		}
		if rerr == io.EOF {
			return s.removed, nil
		}
		if rerr != nil {
			return 0, rerr
		}
	}
}

// thinkingStripper is StripThinkingStream one line at a time.
type thinkingStripper struct {
	redirect map[string]json.RawMessage // dropped line's uuid -> parentUuid to use instead
	removed  int
}

func newThinkingStripper() *thinkingStripper {
	return &thinkingStripper{redirect: map[string]json.RawMessage{}}
}

// line returns what seg, a line with its end of line, becomes: seg itself, its
// rewritten form, or nil when it is dropped.
func (s *thinkingStripper) line(seg []byte) ([]byte, error) {
	out, note := preStrip(seg)
	return s.finish(out, note)
}

// stripNote is what preStrip leaves for finish.
type stripNote struct {
	removed int
	drop    bool   // the line had nothing but thinking
	uuid    string // of a dropped line, when it has one
	hasUUID bool
	parent  json.RawMessage // of a dropped line
	err     error
	// links are the values of the line's link keys (spans of the line); a
	// value that is not a plain string makes mustParse.
	links     [][2]int
	mustParse bool
}

// preStrip removes the thinking blocks of seg. It keeps no state, so it runs
// on any goroutine; the links to dropped lines are finish's, in order.
func preStrip(seg []byte) ([]byte, any) {
	line := bytes.TrimRight(seg, "\r\n")
	if len(line) == 0 || line[0] != '{' {
		return seg, nil
	}
	out, n := seg, (*stripNote)(nil)
	if bytes.Contains(line, thinkingMark) {
		if out, n = stripBlocks(seg, line); n != nil && (n.drop || n.err != nil) {
			return out, n
		}
	}
	links, must := linkSpans(bytes.TrimRight(out, "\r\n"))
	if len(links) == 0 && !must {
		if n == nil {
			return out, nil
		}
		return out, n
	}
	if n == nil {
		n = &stripNote{}
	}
	n.links, n.mustParse = links, must
	return out, n
}

// linkSpans finds the values of the link keys in line, as linksTo reads them.
func linkSpans(line []byte) (spans [][2]int, mustParse bool) {
	for _, k := range linkMarks {
		for at := 0; ; {
			i := bytes.Index(line[at:], k)
			if i < 0 {
				break
			}
			at += i + len(k)
			v := at
			for v < len(line) && (line[v] == ' ' || line[v] == '\t') {
				v++
			}
			if v >= len(line) || line[v] != ':' {
				continue
			}
			v++
			for v < len(line) && (line[v] == ' ' || line[v] == '\t') {
				v++
			}
			if v >= len(line) || line[v] != '"' {
				continue
			}
			v++
			end := bytes.IndexByte(line[v:], '"')
			if end < 0 || bytes.IndexByte(line[v:v+end], '\\') >= 0 {
				mustParse = true // malformed, or escaped: not a plain uuid
				continue
			}
			spans = append(spans, [2]int{v, v + end})
		}
	}
	return spans, mustParse
}

// linkMarks are the link keys as they stand in a line.
var linkMarks = func() [][]byte {
	out := make([][]byte, len(linkKeys))
	for i, k := range linkKeys {
		out[i] = []byte(`"` + k + `"`)
	}
	return out
}()

// stripBlocks is preStrip's removal of the thinking blocks of line (seg
// without its end of line); a nil note: nothing to remove.
func stripBlocks(seg, line []byte) ([]byte, *stripNote) {
	var top map[string]json.RawMessage
	if json.Unmarshal(line, &top) != nil {
		return seg, nil // not ours to judge
	}
	var msg map[string]json.RawMessage
	var blocks []json.RawMessage
	if json.Unmarshal(top["message"], &msg) != nil || json.Unmarshal(msg["content"], &blocks) != nil || len(blocks) == 0 {
		return seg, nil
	}
	kept := make([]json.RawMessage, 0, len(blocks))
	removed := 0
	for _, b := range blocks {
		if isThinking(b) {
			removed++
		} else {
			kept = append(kept, b)
		}
	}
	if removed == 0 {
		return seg, nil
	}
	if len(kept) == 0 {
		n := &stripNote{removed: removed, drop: true, parent: top["parentUuid"]}
		if raw, ok := top["uuid"]; ok && json.Unmarshal(raw, &n.uuid) == nil {
			n.hasUUID = true
		}
		return nil, n
	}
	var err error
	if msg["content"], err = encode(kept); err == nil {
		if top["message"], err = encode(msg); err == nil {
			var out json.RawMessage
			if out, err = encode(top); err == nil {
				return append(out, seg[len(line):]...), &stripNote{removed: removed}
			}
		}
	}
	return seg, &stripNote{err: err}
}

// finish completes preStrip's work on a line, in the order of the lines: it
// remembers a dropped line and points the links to it at its parent.
func (s *thinkingStripper) finish(seg []byte, note any) ([]byte, error) {
	n, _ := note.(*stripNote)
	if n == nil {
		return seg, nil // nothing removed, no link to look at
	}
	{
		if n.err != nil {
			return nil, n.err
		}
		s.removed += n.removed
		if n.drop {
			if n.hasUUID {
				parent := n.parent
				var id string
				if parent != nil && json.Unmarshal(parent, &id) == nil {
					if to, ok := s.redirect[id]; ok {
						parent = to
					}
				}
				if parent == nil {
					parent = json.RawMessage("null")
				}
				s.redirect[n.uuid] = parent
			}
			return nil, nil
		}
	}
	if len(s.redirect) == 0 {
		return seg, nil
	}
	hit := n.mustParse
	for _, sp := range n.links {
		if _, ok := s.redirect[string(seg[sp[0]:sp[1]])]; ok {
			hit = true
			break
		}
	}
	if !hit {
		return seg, nil
	}
	line := bytes.TrimRight(seg, "\r\n")
	var top map[string]json.RawMessage
	if json.Unmarshal(line, &top) != nil {
		return seg, nil
	}
	changed := false
	for _, k := range linkKeys {
		var id string
		if raw, ok := top[k]; ok && json.Unmarshal(raw, &id) == nil {
			if to, ok := s.redirect[id]; ok {
				top[k] = to
				changed = true
			}
		}
	}
	if !changed {
		return seg, nil
	}
	out, err := encode(top)
	if err != nil {
		return nil, err
	}
	return append(out, seg[len(line):]...), nil
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
