package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// roundTripFunc is an http.RoundTripper made of a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The header values that go upstream must not alias fasthttp's request buffer:
// the HTTP/2 transport keeps them in its HPACK table, and strings that change
// under it panic the server ("id (N) <= evictCount (M)", in a goroutine of
// net/http that cannot be recovered). A value read from the request must still
// be what it was after later requests have been parsed.
func TestForwardedHeaderStringsDoNotAliasTheRequestBuffer(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		t.Run(path, func(t *testing.T) {
			var kept []string // what the transport was given, as it was given
			old := upstreamClient
			upstreamClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				kept = append(kept, r.Header.Get("anthropic-beta"), r.Header.Get("anthropic-version"), r.Header.Get("Accept"))
				return &http.Response{
					StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{}`)), Request: r,
				}, nil
			})}
			t.Cleanup(func() { upstreamClient = old })

			app := compatApp(compatConfig("http://upstream.invalid/api", nil))
			call := func(beta, version, accept string) {
				t.Helper()
				req := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"m","messages":[]}`))
				req.Header.Set("anthropic-beta", beta)
				req.Header.Set("anthropic-version", version)
				req.Header.Set("Accept", accept)
				resp, err := app.Test(req)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
			}
			// Values of the same lengths, so that a buffer reused in place is overwritten.
			call("beta-aaaaaaaaaaaaaaaaaa", "2023-06-01", "text/event-stream")
			call("beta-bbbbbbbbbbbbbbbbbb", "2024-01-01", "application/json")
			call("beta-cccccccccccccccccc", "2025-02-02", "text/plain-xxxxxx")

			want := []string{
				"beta-aaaaaaaaaaaaaaaaaa", "2023-06-01", "text/event-stream",
				"beta-bbbbbbbbbbbbbbbbbb", "2024-01-01", "application/json",
				"beta-cccccccccccccccccc", "2025-02-02", "text/plain-xxxxxx",
			}
			if len(kept) != len(want) {
				t.Fatalf("the transport saw %d values, want %d: %q", len(kept), len(want), kept)
			}
			for i := range want {
				if kept[i] != want[i] {
					t.Errorf("value %d changed after the request: %q, want %q", i, kept[i], want[i])
				}
			}
		})
	}
}

func TestOwnStringIsACopy(t *testing.T) {
	b := []byte("abc")
	s := ownString(string(b))
	b[0] = 'x'
	if s != "abc" {
		t.Errorf("ownString = %q", s)
	}
}

// fiberStringMethods return strings that alias the request buffer.
var fiberStringMethods = map[string]bool{
	"Get": true, "Params": true, "Query": true, "Cookies": true, "FormValue": true,
	"Hostname": true, "IP": true, "Path": true, "OriginalURL": true, "BaseURL": true,
}

// A string read from the fiber context may be kept (in a variable, a header, a
// map) only as a copy: every such call whose result is assigned must be wrapped
// in ownString. Using one in place (a comparison, an argument) is fine: it is
// gone when the handler returns. The check reads the source of this package.
func TestFiberStringsAreCopiedBeforeTheyAreKept(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		unsafeCall := func(e ast.Expr) (string, bool) {
			call, ok := e.(*ast.CallExpr)
			if !ok {
				return "", false
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !fiberStringMethods[sel.Sel.Name] {
				return "", false
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "c" {
				return "", false
			}
			return sel.Sel.Name, true
		}
		report := func(rhs []ast.Expr) {
			for _, r := range rhs {
				if m, bad := unsafeCall(r); bad {
					t.Errorf("%s: c.%s(...) is assigned without ownString: the string aliases the request buffer",
						fset.Position(r.Pos()), m)
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				report(n.Rhs)
			case *ast.ValueSpec:
				report(n.Values)
			case *ast.KeyValueExpr:
				report([]ast.Expr{n.Value})
			case *ast.CallExpr:
				// A string put into a header or a map-like setter.
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Set" || sel.Sel.Name == "Add") && len(n.Args) == 2 {
					report(n.Args[1:])
				}
			}
			return true
		})
	}
	if checked < 5 {
		t.Fatalf("only %d source files read: the check does not see the package", checked)
	}
}
