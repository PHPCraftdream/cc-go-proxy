package sessions

import (
	"bytes"
	"encoding/json"
)

// refStripLine is the one-pass strip that preStrip and finish replace, kept as
// the reference they must agree with.
func refStripLine(line []byte, redirect map[string]json.RawMessage) (rewritten []byte, drop bool, removed int, err error) {
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

// refStrip is StripThinking as it was, line by line with refStripLine.
func refStrip(data []byte) ([]byte, int) {
	redirect := map[string]json.RawMessage{}
	var out bytes.Buffer
	removed := 0
	for _, seg := range bytes.SplitAfter(data, []byte("\n")) {
		if len(seg) == 0 {
			continue
		}
		line := bytes.TrimRight(seg, "\r\n")
		rewritten, drop, n, err := refStripLine(line, redirect)
		if err != nil {
			panic(err)
		}
		removed += n
		switch {
		case drop:
		case rewritten != nil:
			out.Write(append(rewritten, seg[len(line):]...))
		default:
			out.Write(seg)
		}
	}
	return out.Bytes(), removed
}
