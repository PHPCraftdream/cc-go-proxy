package codex

// ItemParts reads one input item: its type, role, message text and the number
// of content parts. The four converter structs are read from their Go type;
// a map from its "type"/"role"/"content" fields; anything else is empty.
func ItemParts(item any) (typ, role, text string, parts int) {
	switch v := item.(type) {
	case messageItem:
		var b []byte
		for _, part := range v.Content {
			b = append(b, part.Text...)
		}
		return "message", v.Role, string(b), len(v.Content)
	case functionCallItem:
		return "function_call", "", "", 0
	case functionOutputItem:
		return "function_call_output", "", "", 0
	case reasoningItem:
		return "reasoning", "", "", 0
	case map[string]any:
		typ, _ = v["type"].(string)
		role, _ = v["role"].(string)
		content, _ := v["content"].([]any)
		parts = len(content)
		var b []byte
		for _, part := range content {
			m, _ := part.(map[string]any)
			t, _ := m["text"].(string)
			b = append(b, t...)
		}
		return typ, role, string(b), parts
	}
	return "", "", "", 0
}
