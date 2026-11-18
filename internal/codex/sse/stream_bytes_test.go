package sse

import (
	"strings"
	"testing"
)

func TestRunTrimsSSEDataWhitespaceBeforeRetainingDetail(t *testing.T) {
	const payload = `{ "type" : "response.failed", "response" : { "error" : { "message" : "failed" } } }`
	line := "data:\t  " + payload + " \t\n\n"
	tr := &Translator{Emit: func(string, any) {}}
	if err := tr.Run(strings.NewReader(line)); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if tr.Detail != payload {
		t.Errorf("Detail = %q, want trimmed payload %q", tr.Detail, payload)
	}
}

func TestRunAcceptsLineOverOld16MiBLimit(t *testing.T) {
	const deltaLen = 17 << 20 // above the old 16 MiB limit, well under wsMaxSSELine (~32 MiB+7)
	delta := strings.Repeat("x", deltaLen)
	// The text block is opened by its item, and the stream ends with a completion.
	stream := "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"id\":\"i\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"i\",\"delta\":\"" + delta + "\"}\n\n" +
		"data: {\"type\":\"response.completed\"}\n\n"

	var text string
	tr := &Translator{Emit: func(_ string, payload any) {
		m, ok := payload.(map[string]any)
		if !ok {
			return
		}
		if d, ok := m["delta"].(map[string]any); ok {
			if s, ok := d["text"].(string); ok {
				text += s
			}
		}
	}}
	if err := tr.Run(strings.NewReader(stream)); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if tr.Failed != "" {
		t.Errorf("Failed = %q, want empty", tr.Failed)
	}
	if len(text) != deltaLen {
		t.Errorf("emitted text delta length = %d, want %d", len(text), deltaLen)
	}
}

func TestRunRejectsLineOverWSMaxSSELine(t *testing.T) {
	// Enough inside the delta that the whole line exceeds wsMaxSSELine.
	payloadLen := int(wsMaxSSELine) + 16
	line := "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"i\",\"delta\":\"" +
		strings.Repeat("x", payloadLen) + "\"}\n\n"

	var emittedError bool
	tr := &Translator{Emit: func(kind string, _ any) {
		if kind == "error" {
			emittedError = true
		}
	}}
	err := tr.Run(strings.NewReader(line))
	if err == nil {
		t.Fatal("Run() error = nil, want scanner limit error")
	}
	if !strings.Contains(err.Error(), "token too long") {
		t.Errorf("Run() error = %v, want it to contain %q", err, "token too long")
	}
	if tr.Failed == "" {
		t.Error("Failed is empty, want the stream failure message")
	}
	if !emittedError {
		t.Error("no error event emitted")
	}
}
