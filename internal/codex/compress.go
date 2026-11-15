package codex

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

// A request carries the whole conversation again and again, several MiB of
// JSON, and the uplink is the slow part of a long turn (12-17 s for 6 MiB seen).
// The official Codex client sends the body zstd-compressed to this backend, so
// ccgp does the same: JSON shrinks five- to ten-fold.

// compressThreshold is the body size from which it is compressed; a smaller
// one gains nothing.
var compressThreshold = 32 << 10

// Keep the endpoint refusal and acceptance caches bounded.
const (
	maxRefusedEndpoints  = 128
	maxAcceptedEndpoints = 128
)

// endpointMemory remembers one fact per endpoint, bounded: the oldest entry
// (smallest sequence) is dropped when one more is needed.
type endpointMemory struct {
	sync.Mutex
	endpoints map[string]uint64
	next      uint64
	max       int
}

func newEndpointMemory(max int) *endpointMemory {
	return &endpointMemory{endpoints: make(map[string]uint64), max: max}
}

var (
	compressionRefusals    = newEndpointMemory(maxRefusedEndpoints)
	compressionAcceptances = newEndpointMemory(maxAcceptedEndpoints)
)

func normalizedEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	path := strings.TrimRight(strings.ToLower(u.Path), "/")
	if path == "/responses" {
		path = ""
	} else {
		path = strings.TrimSuffix(path, "/responses")
	}
	path = strings.TrimRight(path, "/")
	return scheme + "://" + host + path
}

func (m *endpointMemory) has(raw string) bool {
	endpoint := normalizedEndpoint(raw)
	if endpoint == "" {
		return false
	}
	m.Lock()
	defer m.Unlock()
	_, ok := m.endpoints[endpoint]
	return ok
}

func (m *endpointMemory) learn(raw string) (string, bool) {
	endpoint := normalizedEndpoint(raw)
	if endpoint == "" {
		return "", false
	}
	m.Lock()
	defer m.Unlock()
	if _, ok := m.endpoints[endpoint]; ok {
		return endpoint, false
	}
	if len(m.endpoints) >= m.max {
		var oldest string
		var oldestSequence uint64
		for key, sequence := range m.endpoints {
			if oldest == "" || sequence < oldestSequence {
				oldest, oldestSequence = key, sequence
			}
		}
		delete(m.endpoints, oldest)
	}
	m.next++
	m.endpoints[endpoint] = m.next
	return endpoint, true
}

func compressionRefused(raw string) bool {
	return compressionRefusals.has(raw)
}

func refuseCompression(raw string) (string, bool) {
	return compressionRefusals.learn(raw)
}

func compressionAccepted(raw string) bool {
	return compressionAcceptances.has(raw)
}

func acceptCompression(raw string) (string, bool) {
	return compressionAcceptances.learn(raw)
}

// encoder is shared: EncodeAll is safe for concurrent use.
type bodyEncoder interface {
	EncodeAll([]byte, []byte) []byte
}

var encoder bodyEncoder = func() *zstd.Encoder {
	e, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		panic(err) // only for bad options
	}
	return e
}()

// Notice, when set, hears the rare events of the compression (a log line).
var Notice func(msg string)

// Wire says what Call put on the wire; the caller hands one in with WithWire.
type Wire struct {
	Raw      int    // the JSON body
	Sent     int    // what was sent
	Encoding string // "zstd", or "" for the plain body
}

type wireKey struct{}

// WithWire makes Call fill w.
func WithWire(ctx context.Context, w *Wire) context.Context {
	return context.WithValue(ctx, wireKey{}, w)
}

func wireFrom(ctx context.Context) *Wire {
	w, _ := ctx.Value(wireKey{}).(*Wire)
	return w
}

// sendPrepared posts the prepared body, compressed when it is big enough. A 400 or 415 for a
// compressed body is asked again plain; only when that is accepted is the
// compression given up for good (a genuinely bad request fails both ways). An
// endpoint that has taken a compressed body once keeps it: its later 400 or
// 415 is the request's own error and goes back to the caller as it is.
func sendPrepared(ctx context.Context, url string, p *Prepared, tok *oauth.Tokens) (*http.Response, error) {
	body, hdr := p.body, p.headers
	w := wireFrom(ctx)
	if w != nil {
		*w = Wire{Raw: len(body), Sent: len(body)}
	}
	if len(body) < compressThreshold || (compressionRefused(url) && !compressionAccepted(url)) {
		return post(ctx, url, body, "", hdr, tok)
	}
	packed := p.compressed()
	if w != nil {
		w.Sent, w.Encoding = len(packed), "zstd"
	}
	resp, err := post(ctx, url, packed, "zstd", hdr, tok)
	if err == nil && resp.StatusCode/100 == 2 {
		acceptCompression(url) // a later 400/415 is then the request's own error
	}
	if err != nil || (resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusUnsupportedMediaType) {
		return resp, err
	}
	if compressionAccepted(url) { // this endpoint takes compressed bodies: the 400/415 is the request's own error
		return resp, err
	}
	refused := resp.StatusCode
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if w != nil {
		w.Sent, w.Encoding = len(body), ""
	}
	resp, err = post(ctx, url, body, "", hdr, tok)
	if err == nil && resp.StatusCode/100 == 2 {
		if endpoint, first := refuseCompression(url); first && Notice != nil {
			Notice(fmt.Sprintf("codex: the backend at %s refused a compressed body (HTTP %d) and took it plain: no compression for this endpoint", endpoint, refused))
		}
	}
	return resp, err
}
