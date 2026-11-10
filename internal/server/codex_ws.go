package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
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
		codexWSManager = newCodexWSManager()
	}
	return codexWSManager
}

// newCodexWSManager is a manager whose learned limits and side messages are logged.
func newCodexWSManager() *codex.WSManager {
	m := codex.NewWSManager()
	m.OnLimit = logWSLimit
	m.OnSide = logWSSide
	m.OnLost = logWSLost
	return m
}

func codexWSStatsManager() *codex.WSManager {
	codexWSManagerMu.Lock()
	defer codexWSManagerMu.Unlock()
	return codexWSManager
}

func safeWSReason(reason string) string {
	switch reason {
	case "new connection", "connection broken", "connection too old", "lowered age limit", "no chain", "no response id", "cache key changed", "model changed", "tools changed", "system changed", "reasoning changed", "tool choice changed", "nothing new", "output mismatch",
		"output missing", "assistant text changed", "reasoning dropped", "reasoning differs", "tool call changed",
		"output unreadable", "output not an assistant message", "output reasoning without content", "output tool call without id", "delta copy failed":
		return reason
	}
	if wsDivergence.MatchString(reason) || wsToolChange.MatchString(reason) || wsItemKind.MatchString(reason) {
		return reason
	}
	if strings.HasPrefix(reason, "history diverged at item ") {
		return "history diverged"
	}
	return "full request"
}

// The numbers of a divergence and the name of the tool whose call changed are
// safe to log; nothing else of the history is.
var (
	wsDivergence = regexp.MustCompile(`^history diverged at item \d+( of \d+)?$`)
	wsToolChange = regexp.MustCompile(`^tool call changed \((id|name|arguments)(, ([A-Za-z0-9_.:-]{1,40}|\?)(, was \d{1,9} B, now \d{1,9} B, first \d{1,9} B and last \d{1,9} B equal)?)?\)$`)
	wsItemKind   = regexp.MustCompile(`^((assistant message|tool call) missing \(found [a-z_]{1,32}( message)?\)|new item not sendable \([a-z_]{1,32}( message)?\)|output item not compared \([a-z_]{1,32}\))$`)
)

// wsConvTag names a websocket conversation (one connection and its chain) in
// the log: the start of its thread id, which is a hash.
func wsConvTag(threadID string) string {
	if len(threadID) < 6 {
		return threadID
	}
	return threadID[:6]
}

// wsAgentTag is the agent's part of a conversation tag: ".1a2b" (empty without one).
func wsAgentTag(agent string) string {
	if len(agent) < 4 {
		return ""
	}
	return "." + agent[:4]
}

// logWSLost tells that a connection was ended by the backend or the network:
// when, with which close status, and whether it counts toward lowering the
// planned age (a connection younger than 5 minutes says nothing of a limit).
func logWSLost(l codex.WSLost) {
	closeText := "no close frame"
	if l.Status >= 0 {
		closeText = fmt.Sprintf("close status %d", l.Status)
	}
	verdict := "too young to say anything about a limit"
	switch {
	case l.Counts:
		verdict = "before the planned age, counts toward lowering it"
	case l.Age >= l.Planned:
		verdict = "at or after the planned age"
	}
	_, _ = fmt.Fprintf(logOut, "[%s] upstream websocket [codex]: a connection was ended by the backend or the network at %s of age (%s; planned %s): %s\n",
		time.Now().Format("15:04:05"), roundDur(l.Age), closeText, roundDur(l.Planned), verdict)
}

func (u *upstreamTrace) convText() string {
	if u.conv == "" {
		return ""
	}
	return ", conv " + u.conv
}

func (u *upstreamTrace) convPrefix() string {
	if u.conv == "" {
		return ""
	}
	return " conv " + u.conv
}

// wsDivergenceText says where a request's history left the chain it would have
// continued: the item, what it was and is now (kind and size, never content),
// where the chain's reply is in the new history, and the prompt comparison.
func wsDivergenceText(d codex.WSDivergence, prompt string) string {
	text := fmt.Sprintf("history diverged at item %d of %d (this request has %d): was %s, ", d.At, d.Had, d.Has, shapeText(d.Was))
	if d.Now.Kind == "" {
		text += "now the request ends there"
	} else {
		text += "now " + shapeText(d.Now)
		if diff := d.Now.Size - d.Was.Size; diff > 0 {
			text += " (+" + sizeString(diff) + ")"
		} else if diff < 0 {
			text += " (-" + sizeString(-diff) + ")"
		}
	}
	if d.SameHead >= 0 {
		text += fmt.Sprintf("; first %s and last %s equal, between them %s -> %s", sizeString(d.SameHead), sizeString(d.SameTail),
			sizeString(d.Was.Size-d.SameHead-d.SameTail), sizeString(d.Now.Size-d.SameHead-d.SameTail))
	}
	switch {
	case !d.Reply:
		text += "; the chain has no reply"
	case d.ReplyAt >= 0:
		text += fmt.Sprintf("; the reply chained on is at item %d", d.ReplyAt)
	default:
		text += "; the reply chained on is not in this request"
	}
	if d.Since > 0 {
		text += "; the chained reply ended " + roundDur(d.Since) + " ago"
	}
	if prompt != "" {
		text += " | " + prompt
	}
	return text
}

func shapeText(s codex.ItemShape) string {
	if s.Kind == "" {
		return "nothing"
	}
	text := s.Kind + " " + sizeString(s.Size)
	if s.Parts > 0 {
		text += " in " + count(s.Parts, "part", "parts")
	}
	return text
}

// logWSLimit tells that the planned age of connections was lowered because the
// backend ended them early, again and again.
func logWSLimit(l codex.WSLimit) {
	closeText := "no close frame"
	if l.Status >= 0 {
		closeText = fmt.Sprintf("close status %d", l.Status)
	}
	_, _ = fmt.Fprintf(logOut, "[%s] upstream websocket [codex]: the backend ended connections before the planned %s (the last at %s, %s); replacing them after %s from now on\n",
		time.Now().Format("15:04:05"), roundDur(l.Was), roundDur(l.Broke), closeText, roundDur(l.Age))
}

// logWSSide tells that a message was learned as a background request's, so
// requests ending in it now go over HTTP.
func logWSSide(s codex.WSSide) {
	_, _ = fmt.Fprintf(logOut, "[%s] upstream websocket [codex]: a user message of %s was answered on a conversation's websocket and its reply was left out of the next request (a background request of the client); requests ending in it go over HTTP from now on, so they do not replace the websocket chain\n",
		time.Now().Format("15:04:05"), sizeString(s.Size))
}

// noteSideRequest marks a request that skips the websocket because it is a
// background one.
func (u *upstreamTrace) noteSideRequest(bodyBytes int) {
	u.mu.Lock()
	u.transport = "transport http (side request)"
	u.httpNote = u.httpNoteLocked(bodyBytes)
	u.mu.Unlock()
}

// httpNoteLocked names the conversation and the size of a request that went
// over HTTP in place of the websocket, so its cost can be told per agent.
func (u *upstreamTrace) httpNoteLocked(bodyBytes int) string {
	conv := ""
	if u.conv != "" {
		conv = "conv " + u.conv
	}
	size := ""
	if bodyBytes > 0 {
		size = "request " + sizeString(bodyBytes)
	}
	return joinNonEmpty(conv, size)
}

// wsReasonText is the reason for the request line; a replacement at a lowered
// age says which age.
func wsReasonText(info codex.WSInfo) string {
	reason := safeWSReason(info.Reason)
	if reason == "lowered age limit" && info.Limit > 0 {
		return "replaced at the lowered age of " + roundDur(info.Limit)
	}
	return reason
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
	u.wsConn = "conn new (dial " + roundDur(info.DialTime) + u.convText() + ")"
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
		transport = fmt.Sprintf("transport websocket (full: %s, sent %s)", wsReasonText(info), frameSize)
	}
	conn := "conn new (dial " + roundDur(info.DialTime) + u.convText() + ")"
	if info.Reused {
		conn = "conn reused (age " + roundDur(info.ConnAge) + u.convText() + ")"
	}
	if info.Divergence != nil {
		_, _ = fmt.Fprintf(logOut, "[%s] upstream websocket [codex]%s: %s\n", time.Now().Format("15:04:05"),
			u.convPrefix(), wsDivergenceText(*info.Divergence, u.prompt))
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

func (u *upstreamTrace) noteWSFallback(reason string, bodyBytes int) {
	u.mu.Lock()
	u.httpNote = u.httpNoteLocked(bodyBytes)
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

// requestCall sends the request over the websocket, or over HTTP when the
// websocket fails before the request reached the backend. Where a request may
// go again:
//   - an error from the manager means the backend has no complete request: the
//     dial failed, or the frame's write did (a websocket message is
//     length-framed, the backend does not act on a part of one). HTTP sends it.
//   - the frame went out and the connection broke before any content: runUpstream
//     asks again (brokenBackoff), the same rule as for an HTTP request broken
//     before its reply. At worst the backend ran it twice; the client sees one.
//   - content reached the client: the stream ends with an error, never a second
//     inference (codex.Translator allows no retry after the first block).
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
		trace.noteWSFallback(safeWSFallbackReason(err), bodyBytes)
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
	fallbacks := codexWSFallbacks.Swap(0)
	if stats.Delta+stats.Full+stats.Busy+stats.Fallbacks+int(fallbacks)+stats.Dials+stats.Lost+stats.Early+stats.Side == 0 && stats.SentBytes == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "[%s] upstream websocket [codex]: %s\n", now.Format("15:04:05"), wsStatsText(stats, int(fallbacks)))
}

// wsStatsText is the body of the periodic websocket line; fallbacks counts the
// requests that went over HTTP, stats.Fallbacks the full ones a delta could not
// continue.
func wsStatsText(stats codex.WSStats, fallbacks int) string {
	full := count(stats.Full, "full", "full")
	if stats.Fallbacks > 0 {
		full += fmt.Sprintf(" (%d where a delta could not continue)", stats.Fallbacks)
	}
	text := fmt.Sprintf("%s, %s, %s, %s, %s; sent %s",
		count(stats.Delta, "delta", "delta"), full,
		count(stats.Busy, "busy", "busy"), count(fallbacks, "HTTP fallback", "HTTP fallbacks"), count(stats.Dials, "dial", "dials"),
		sizeString(int(stats.SentBytes)))
	if stats.Lost > 0 {
		text += "; " + count(stats.Lost, "connection", "connections") + " ended by the backend or the network"
	}
	if stats.Early > 0 {
		text += "; " + count(stats.Early, "connection", "connections") + " replaced at the lowered age"
	}
	if stats.Side > 0 {
		text += "; " + count(stats.Side, "background request", "background requests") + " sent over HTTP"
	}
	return text
}
