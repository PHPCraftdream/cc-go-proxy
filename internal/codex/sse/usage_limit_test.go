package sse

import (
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
)

func TestIsUsageLimit(t *testing.T) {
	for body, want := range map[string]bool{
		`{"error":{"type":"usage_limit_reached","message":"x"}}`: true,
		`{"error":{"code":"usage_limit_reached"}}`:               true,
		`{"error":{"type":"rate_limit_exceeded"}}`:               false,
		`{"error":"usage_limit_reached"}`:                        false,
		`not json`:                                               false,
	} {
		if got := codex.IsUsageLimit([]byte(body)); got != want {
			t.Errorf("IsUsageLimit(%s) = %v, want %v", body, got, want)
		}
	}
}

func TestFailedCodeNamesTheUpstreamError(t *testing.T) {
	for in, want := range map[string]string{
		`{"type":"error","error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`: codex.UsageLimitCode,
		`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"m"}}}`:         "rate_limit_exceeded",
		`{"type":"error","message":"no code"}`: "",
	} {
		tr := &Translator{Emit: func(string, any) {}}
		_ = tr.Run(strings.NewReader("data: " + in + "\n\n"))
		if tr.Failed == "" || tr.FailedCode != want {
			t.Errorf("%s: Failed %q, FailedCode %q, want %q", in, tr.Failed, tr.FailedCode, want)
		}
	}
}
