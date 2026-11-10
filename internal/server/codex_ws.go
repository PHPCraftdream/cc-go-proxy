package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
)

var (
	codexWSManagerMu sync.Mutex
	codexWSManager   *codex.WSManager
	codexWSFallbacks atomic.Int64
)

func currentCodexWSManager() *codex.WSManager {
	codexWSManagerMu.Lock()
	defer codexWSManagerMu.Unlock()
	if codexWSManager == nil {
		codexWSManager = codex.NewWSManager()
	}
	return codexWSManager
}

func codexWSStatsManager() *codex.WSManager {
	codexWSManagerMu.Lock()
	defer codexWSManagerMu.Unlock()
	return codexWSManager
}

func safeWSReason(reason string) string {
	switch reason {
	case "new connection", "connection broken", "connection too old", "no chain", "no response id", "cache key changed", "model changed", "tools changed", "system changed", "reasoning changed", "tool choice changed", "nothing new", "output mismatch",
		"output missing", "assistant text changed", "reasoning dropped", "reasoning differs", "tool call changed":
		return reason
	}
	if strings.HasPrefix(reason, "history diverged at item ") {
		return "history diverged"
	}
	return "full request"
}

func safeWSFallbackReason(err error) string {
	if err == nil {
		return "connection failed"
	}
	const handshakePrefix = "handshake returned HTTP "
	if at := strings.Index(err.Error(), handshakePrefix); at >= 0 {
		var status int
		if _, scanErr := fmt.Sscanf(err.Error()[at+len(handshakePrefix):], "%d", &status); scanErr == nil && status >= 100 && status <= 599 {
			return fmt.Sprintf("handshake HTTP %d", status)
		}
	}
	var handshake *codex.WSHandshakeError
	if errors.As(err, &handshake) && handshake.Status >= 100 && handshake.Status <= 599 {
		return fmt.Sprintf("handshake HTTP %d", handshake.Status)
	}
	switch {
	case errors.Is(err, codex.ErrWSBusy):
		return "connection busy"
	case errors.Is(err, codex.ErrWSToken):
		return "no usable login"
	case errors.Is(err, codex.ErrWSUnavailable):
		return "unavailable"
	case errors.Is(err, context.Canceled):
		return "request canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "request deadline exceeded"
	}
	return "connection failed"
}

func (u *upstreamTrace) startWSAttempt() {
	u.mu.Lock()
	u.calls++
	u.attempt = time.Now()
	u.got = time.Time{}
	u.wrote = time.Time{}
	u.headers = time.Time{}
	u.read = time.Time{}
	u.mu.Unlock()
}

func (u *upstreamTrace) noteWSFailure(info codex.WSInfo, err error) {
	u.mu.Lock()
	u.transport = "transport websocket (full: " + safeWSFallbackReason(err) + ")"
	u.wsConn = "conn new (dial " + roundDur(info.DialTime) + ")"
	u.wire.Raw = 0
	u.wire.Sent = 0
	u.wire.Encoding = ""
	u.mu.Unlock()
}

func (u *upstreamTrace) noteWebsocket(info codex.WSInfo, bodyBytes int) {
	frameSize := sizeString(info.SentBytes)
	var transport string
	if info.Mode == "delta" {
		transport = fmt.Sprintf("transport websocket (delta: sent %s of %s)", frameSize, sizeString(bodyBytes))
	} else {
		transport = fmt.Sprintf("transport websocket (full: %s, sent %s)", safeWSReason(info.Reason), frameSize)
	}
	conn := "conn new (dial " + roundDur(info.DialTime) + ")"
	if info.Reused {
		conn = "conn reused (age " + roundDur(info.ConnAge) + ")"
	}
	u.mu.Lock()
	u.transport = transport
	u.wsConn = conn
	u.wire.Raw = info.SentBytes
	u.wire.Sent = info.SentBytes
	u.wire.Encoding = "ws"
	u.got = time.Time{}
	u.wrote = time.Time{}
	u.headers = time.Time{}
	u.read = time.Time{}
	u.reused = false
	u.idle = 0
	u.warm = false
	u.tls = 0
	u.mu.Unlock()
}

func (u *upstreamTrace) noteWSFallback(reason string) {
	u.mu.Lock()
	if u.wire != nil && u.wire.Encoding == "ws" {
		u.wire.Raw = 0
		u.wire.Sent = 0
		u.wire.Encoding = ""
	}
	u.transport = "fallback to HTTP (" + reason + ")"
	u.wsConn = ""
	u.mu.Unlock()
	codexWSFallbacks.Add(1)
}

func (u *upstreamTrace) noteWSCanceled(err error) {
	u.mu.Lock()
	u.transport = "transport websocket (" + safeWSFallbackReason(err) + ")"
	u.mu.Unlock()
}

func (r *codexRetry) requestCall(ctx context.Context, trace *upstreamTrace, bodyBytes int) (*http.Response, error) {
	if !r.websocket {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return r.callPrepared()
	}
	trace.startWSAttempt()
	response, info, err := r.callWebsocket()
	if err != nil {
		if ctx.Err() != nil {
			trace.noteWSCanceled(ctx.Err())
			return nil, ctx.Err()
		}
		trace.noteWSFailure(info, err)
		r.websocket = false
		trace.noteWSFallback(safeWSFallbackReason(err))
		return r.callPrepared()
	}
	trace.noteWebsocket(info, bodyBytes)
	return response, nil
}

func (r *codexRetry) callAgain(ctx context.Context, trace *upstreamTrace, bodyBytes int) (*http.Response, error) {
	var response *http.Response
	var err error
	if r.websocket {
		response, err = r.requestCall(ctx, trace, bodyBytes)
	} else {
		response, err = r.callPrepared()
	}
	if err == nil {
		response.Body = trace.stamp(response.Body)
	}
	return response, err
}

func drainCompletedWebsocketReply(retry *codexRetry, tr *codex.Translator, resp *http.Response) {
	if retry != nil && retry.websocket && tr.Failed == "" && resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
}

func writeCodexWebsocketStats(w io.Writer, now time.Time) {
	manager := codexWSStatsManager()
	if manager == nil {
		return
	}
	stats := manager.TakeStats()
	fallbacks := int64(stats.Fallbacks) + codexWSFallbacks.Swap(0)
	if stats.Delta+stats.Full+stats.Busy+int(fallbacks)+stats.Dials == 0 && stats.SentBytes == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "[%s] upstream websocket [codex]: %s, %s, %s, %s, %s; sent %s\n",
		now.Format("15:04:05"), count(stats.Delta, "delta", "delta"), count(stats.Full, "full", "full"),
		count(stats.Busy, "busy", "busy"), count(int(fallbacks), "fallback", "fallbacks"), count(stats.Dials, "dial", "dials"),
		sizeString(int(stats.SentBytes)))
}
