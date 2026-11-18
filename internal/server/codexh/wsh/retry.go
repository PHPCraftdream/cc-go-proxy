package wsh

import (
	"context"
	"errors"
	"net/http"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/sse"
	codexws "github.com/PHPCraftdream/cc-go-proxy/internal/codex/ws"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/obs"
)

// codexRetry owns the immutable upstream request used by the initial call and
// every retry. Its call function receives the prepared value explicitly so it
// cannot retain the converted conversation through a closure.
type CodexRetry struct {
	Request   *codex.Request
	Prepared  *codex.Prepared
	Websocket bool
	Call      func(*codex.Prepared) (*http.Response, error)
	CallWS    func(*codex.Request) (*http.Response, codexws.WSInfo, error)
}

func (r *CodexRetry) CallPrepared() (*http.Response, error) {
	if r.Prepared == nil && r.Request != nil {
		prepared, err := codex.PrepareRequest(r.Request)
		if err != nil {
			return nil, err
		}
		r.Prepared = prepared
	}
	if r.Prepared == nil {
		return nil, errors.New("codex: prepared request unavailable")
	}
	return r.Call(r.Prepared)
}

func (r *CodexRetry) callWebsocket() (*http.Response, codexws.WSInfo, error) {
	if r.Request == nil || r.CallWS == nil {
		return nil, codexws.WSInfo{}, errors.New("codex: websocket request unavailable")
	}
	return r.CallWS(r.Request)
}

func (r *CodexRetry) Emit(next sse.Emit) sse.Emit {
	return func(event string, data any) {
		if event == "content_block_start" {
			r.Request = nil
			r.Prepared = nil
		}
		next(event, data)
	}
}

// requestCall sends the request over the websocket, or over HTTP when the
// websocket fails before the request reached the backend. Where a request may
// go again:
//   - an error from the manager means the backend has no complete request: the
//     dial failed, or the frame's write did (a websocket message is
//     length-framed, the backend does not act on a part of one). HTTP sends it.
//   - the frame went out and the connection broke before any content: runUpstream
//     asks again (BrokenBackoff), the same rule as for an HTTP request broken
//     before its reply. At worst the backend ran it twice; the client sees one.
//   - content reached the client: the stream ends with an error, never a second
//     inference (codex.Translator allows no retry after the first block).
func (r *CodexRetry) RequestCall(ctx context.Context, trace *obs.UpstreamTrace, bodyBytes int) (*http.Response, error) {
	if !r.Websocket {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return r.CallPrepared()
	}
	trace.StartWSAttempt()
	response, info, err := r.callWebsocket()
	if err != nil {
		if ctx.Err() != nil {
			trace.NoteWSCanceled(ctx.Err())
			return nil, ctx.Err()
		}
		trace.NoteWSFailure(info, err)
		r.Websocket = false
		trace.NoteWSFallback(obs.SafeWSFallbackReason(err), bodyBytes)
		return r.CallPrepared()
	}
	trace.NoteWebsocket(info, bodyBytes)
	return response, nil
}

func (r *CodexRetry) CallAgain(ctx context.Context, trace *obs.UpstreamTrace, bodyBytes int) (*http.Response, error) {
	var response *http.Response
	var err error
	if r.Websocket {
		response, err = r.RequestCall(ctx, trace, bodyBytes)
	} else {
		response, err = r.CallPrepared()
	}
	if err == nil {
		response.Body = trace.Stamp(response.Body)
	}
	return response, err
}
