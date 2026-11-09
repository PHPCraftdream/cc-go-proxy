package codex

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
