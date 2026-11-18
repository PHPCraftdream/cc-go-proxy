package wsh

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/sse"
	codexws "github.com/PHPCraftdream/cc-go-proxy/internal/codex/ws"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/obs"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
)

func CurrentCodexWSManager() *codexws.WSManager {
	obs.CodexWSManagerMu.Lock()
	defer obs.CodexWSManagerMu.Unlock()
	if obs.CodexWSManager == nil {
		obs.CodexWSManager = NewCodexWSManager()
	}
	return obs.CodexWSManager
}

// newCodexWSManager is a manager whose learned limits and side messages are logged.
func NewCodexWSManager() *codexws.WSManager {
	m := codexws.NewWSManager()
	m.OnLimit = LogWSLimit
	m.OnSide = logWSSide
	m.OnLost = LogWSLost
	return m
}

func codexWSStatsManager() *codexws.WSManager {
	obs.CodexWSManagerMu.Lock()
	defer obs.CodexWSManagerMu.Unlock()
	return obs.CodexWSManager
}

// wsConvTag names a websocket conversation (one connection and its chain) in
// the log: the start of its thread id, which is a hash.
func WsConvTag(threadID string) string {
	if len(threadID) < 6 {
		return threadID
	}
	return threadID[:6]
}

// wsAgentTag is the agent's part of a conversation tag: ".1a2b" (empty without one).
func WsAgentTag(agent string) string {
	if len(agent) < 4 {
		return ""
	}
	return "." + agent[:4]
}

// logWSLost tells that a connection was ended by the backend or the network:
// when, with which close status, and whether it counts toward lowering the
// planned age (a connection younger than 5 minutes says nothing of a limit).
func LogWSLost(l codexws.WSLost) {
	closeText := "no close frame"
	if l.Status >= 0 {
		closeText = fmt.Sprintf("close status %d", l.Status)
	}
	verdict := "too young to say anything about a limit"
	switch {
	case l.Counts:
		verdict = "before the planned age, counts toward lowering it"
	case l.Burst:
		verdict = "before the planned age, but right after another such break: one event, not counted again"
	case l.Age >= l.Planned:
		verdict = "at or after the planned age"
	}
	_, _ = fmt.Fprintf(shared.LogOut, "[%s] upstream websocket [codex]: a connection was ended by the backend or the network at %s of age (%s; planned %s): %s\n",
		time.Now().Format("15:04:05"), obs.RoundDur(l.Age), closeText, obs.RoundDur(l.Planned), verdict)
}

// logWSLimit tells that the planned age of connections was lowered because the
// backend ended them early, again and again.
func LogWSLimit(l codexws.WSLimit) {
	closeText := "no close frame"
	if l.Status >= 0 {
		closeText = fmt.Sprintf("close status %d", l.Status)
	}
	_, _ = fmt.Fprintf(shared.LogOut, "[%s] upstream websocket [codex]: the backend ended connections before the planned %s (the last at %s, %s); replacing them after %s from now on\n",
		time.Now().Format("15:04:05"), obs.RoundDur(l.Was), obs.RoundDur(l.Broke), closeText, obs.RoundDur(l.Age))
}

// logWSSide tells that a message was learned as a background request's, so
// requests ending in it now go over HTTP.
func logWSSide(s codexws.WSSide) {
	_, _ = fmt.Fprintf(shared.LogOut, "[%s] upstream websocket [codex]: a user message of %s was answered on a conversation's websocket and its reply was left out of the next request (a background request of the client); requests ending in it go over HTTP from now on, so they do not replace the websocket chain\n",
		time.Now().Format("15:04:05"), obs.SizeString(s.Size))
}

// logWSSideWrong tells that a message learned as a background request's was an
// ordinary turn's: the conversation's next request kept it, with its reply.
func LogWSSideWrong(conv string, w codexws.WSSideWrong) {
	_, _ = fmt.Fprintf(shared.LogOut, "[%s] upstream websocket [codex] conv %s: a user message of %s taken for a background request's stayed in the next request, with its reply (an ordinary turn); it is no longer taken for one (%s had gone over HTTP for it)\n",
		time.Now().Format("15:04:05"), conv, obs.SizeString(w.Size), obs.Count(w.Matches, "request", "requests"))
}

func DrainCompletedWebsocketReply(retry *CodexRetry, tr *sse.Translator, resp *http.Response) {
	if retry != nil && retry.Websocket && tr.Failed == "" && resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
}

func WriteCodexWebsocketStats(w io.Writer, now time.Time) {
	manager := codexWSStatsManager()
	if manager == nil {
		return
	}
	stats := manager.TakeStats()
	fallbacks := obs.CodexWSFallbacks.Swap(0)
	if stats.Delta+stats.Full+stats.Busy+stats.Fallbacks+int(fallbacks)+stats.Dials+stats.Lost+stats.Early+stats.Side+stats.SideWrong == 0 && stats.SentBytes == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "[%s] upstream websocket [codex]: %s\n", now.Format("15:04:05"), WsStatsText(stats, int(fallbacks)))
}

// wsStatsText is the body of the periodic websocket line; fallbacks counts the
// requests that went over HTTP, stats.Fallbacks the full ones a delta could not
// continue.
func WsStatsText(stats codexws.WSStats, fallbacks int) string {
	full := obs.Count(stats.Full, "full", "full")
	if stats.Fallbacks > 0 {
		full += fmt.Sprintf(" (%d where a delta could not continue)", stats.Fallbacks)
	}
	text := fmt.Sprintf("%s, %s, %s, %s, %s; sent %s",
		obs.Count(stats.Delta, "delta", "delta"), full,
		obs.Count(stats.Busy, "busy", "busy"), obs.Count(fallbacks, "HTTP fallback", "HTTP fallbacks"), obs.Count(stats.Dials, "dial", "dials"),
		obs.SizeString(int(stats.SentBytes)))
	if stats.Lost > 0 {
		text += "; " + obs.Count(stats.Lost, "connection", "connections") + " ended by the backend or the network"
	}
	if stats.Early > 0 {
		text += "; " + obs.Count(stats.Early, "connection", "connections") + " replaced at the lowered age"
	}
	if stats.Side > 0 {
		text += "; " + obs.Count(stats.Side, "background request", "background requests") + " sent over HTTP"
	}
	if stats.SideWrong > 0 {
		text += "; " + obs.Count(stats.SideWrong, "message", "messages") + " taken for a background request's found to be an ordinary turn's"
	}
	return text
}
