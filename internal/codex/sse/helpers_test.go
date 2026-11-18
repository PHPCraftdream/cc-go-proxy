package sse

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

const overloadedEvent = `{"type":"error","sequence_number":3,"error":{"message":"Our servers are currently overloaded.","type":"service_unavailable_error","code":"server_is_overloaded"}}`

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// sseBody builds an upstream Responses stream.
func sseBody(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("event: x\ndata: " + e + "\n\n")
	}
	return b.String()
}

type errReader struct{ err error }

type failingReader struct{}

var upstreamStream = sseBody(
	`{"type":"response.created","response":{"id":"resp_1"}}`,
	`{"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
	`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"think"}`,
	`{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"ENC"}}`,
	`{"type":"response.output_item.added","item":{"id":"msg_1","type":"message"}}`,
	`{"type":"response.output_text.delta","item_id":"msg_1","delta":"Hel"}`,
	`{"type":"response.output_text.delta","item_id":"msg_1","delta":"lo"}`,
	`{"type":"response.output_item.done","item":{"id":"msg_1","type":"message"}}`,
	`{"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","call_id":"call_9","name":"Read"}}`,
	`{"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","arguments":"{\"path\":\"a\"}"}}`,
	`{"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":40}}}}`,
)

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
