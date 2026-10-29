package codex

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"

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

// noCompress is set once the backend refused a compressed body that it then
// accepted as plain: the rest of the run sends plain bodies.
var noCompress atomic.Bool

// encoder is shared: EncodeAll is safe for concurrent use.
var encoder = func() *zstd.Encoder {
	e, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		panic(err) // only for bad options
	}
	return e
}()

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

// send posts body, compressed when it is big enough. A 400 or 415 for a
// compressed body is asked again plain; only when that is accepted is the
// compression given up for good (a genuinely bad request fails both ways).
func send(ctx context.Context, url string, body []byte, tok *oauth.Tokens) (*http.Response, error) {
	w := wireFrom(ctx)
	if w != nil {
		*w = Wire{Raw: len(body), Sent: len(body)}
	}
	if len(body) < compressThreshold || noCompress.Load() {
		return post(ctx, url, body, "", tok)
	}
	packed := encoder.EncodeAll(body, nil)
	if w != nil {
		w.Sent, w.Encoding = len(packed), "zstd"
	}
	resp, err := post(ctx, url, packed, "zstd", tok)
	if err != nil || (resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusUnsupportedMediaType) {
		return resp, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if w != nil {
		w.Sent, w.Encoding = len(body), ""
	}
	resp, err = post(ctx, url, body, "", tok)
	if err == nil && resp.StatusCode/100 == 2 {
		noCompress.Store(true)
	}
	return resp, err
}
