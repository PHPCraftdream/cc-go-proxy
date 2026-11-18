package codex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

func TestNeedsNewerClient(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		required string
		ok       bool
	}{
		{"backend wording", 400, `{"error":{"message":"The 'gpt-6.3-astra' model requires a newer version of Codex. Please upgrade to the latest app or CLI and try again.","type":"invalid_request_error"}}`, "", true},
		{"version in the text", 400, `{"detail":"Codex 0.171.0 or newer is required: client version is too old"}`, "0.171.0", true},
		{"version field", 426, `{"error":{"message":"unsupported client version","minimal_client_version":[0,172,3]}}`, "0.172.3", true},
		{"string field", 400, `{"error":{"message":"x","minimum_client_version":"0.173.0"}}`, "0.173.0", true},
		{"plan upsell", 403, `{"error":{"message":"Upgrade to Pro to use this model."}}`, "", false},
		{"usage limit", 429, `{"error":{"type":"usage_limit_reached","message":"Upgrade to the latest plan"}}`, "", false},
		{"unauthorized", 401, `{"error":{"message":"requires a newer version"}}`, "", false},
		{"server error", 500, `{"error":{"message":"requires a newer version"}}`, "", false},
		{"other 400", 400, `{"error":{"message":"Unsupported parameter: 'foo'"}}`, "", false},
	}
	for _, tt := range tests {
		required, ok := NeedsNewerClient(tt.status, []byte(tt.body))
		if required != tt.required || ok != tt.ok {
			t.Errorf("%s: got %q, %v; want %q, %v", tt.name, required, ok, tt.required, tt.ok)
		}
	}
}

// Inference and the websocket handshake present the configured version the
// way the official client does (originator/version); without one, as before.
func TestClientVersionInUserAgent(t *testing.T) {
	var agent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent = r.Header.Get("User-Agent")
	}))
	defer srv.Close()
	old := HTTPClient
	HTTPClient = &http.Client{}
	defer func() { HTTPClient = old }()

	tok := &oauth.Tokens{AccessToken: "t"}
	for _, tt := range []struct{ version, want string }{{"0.170.0", "codex_cli_rs/0.170.0 (ccgp)"}, {"", "codex_cli_rs (ccgp)"}} {
		ctx := context.Background()
		if tt.version != "" {
			ctx = WithClientVersion(ctx, tt.version)
		}
		resp, err := post(ctx, srv.URL, []byte(`{}`), "", nil, tok)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if agent != tt.want {
			t.Errorf("HTTP user agent = %q, want %q", agent, tt.want)
		}
		if got := wsHandshakeHeaders(ctx, WSHandshake{Request: &Request{}}, tok).Get("User-Agent"); got != tt.want {
			t.Errorf("websocket user agent = %q, want %q", got, tt.want)
		}
	}
}
