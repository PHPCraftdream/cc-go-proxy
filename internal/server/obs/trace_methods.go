package obs

import (
	"fmt"
	"time"

	codexws "github.com/PHPCraftdream/cc-go-proxy/internal/codex/ws"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
)

// reconnectHook counts a call connpool sent again as a break of the link and
// logs it.
func (u *UpstreamTrace) ReconnectHook(profile, model string) func(n int, err error, since time.Duration, conn, sent string) {
	return func(n int, err error, since time.Duration, conn, sent string) {
		NoteBreak(err.Error(), u.PingHint(err, time.Now()) != "", true)
		LogReconnect(shared.LogOut, profile, model, n, since, err.Error(), conn, sent)
	}
}

func (u *UpstreamTrace) convText() string {
	if u.Conv == "" {
		return ""
	}
	return ", conv " + u.Conv
}

func (u *UpstreamTrace) convPrefix() string {
	if u.Conv == "" {
		return ""
	}
	return " conv " + u.Conv
}

// noteSideRequest marks a request that skips the websocket because it is a
// background one.
func (u *UpstreamTrace) NoteSideRequest(bodyBytes int) {
	u.Mu.Lock()
	u.Transport = "transport http (side request)"
	u.HttpNote = u.httpNoteLocked(bodyBytes)
	u.Mu.Unlock()
}

// httpNoteLocked names the conversation and the size of a request that went
// over HTTP in place of the websocket, so its cost can be told per agent.
func (u *UpstreamTrace) httpNoteLocked(bodyBytes int) string {
	conv := ""
	if u.Conv != "" {
		conv = "conv " + u.Conv
	}
	size := ""
	if bodyBytes > 0 {
		size = "request " + SizeString(bodyBytes)
	}
	return shared.JoinNonEmpty(conv, size)
}

func (u *UpstreamTrace) StartWSAttempt() {
	u.Mu.Lock()
	u.Calls++
	u.Attempt = time.Now()
	u.Got = time.Time{}
	u.Wrote = time.Time{}
	u.Headers = time.Time{}
	u.Read = time.Time{}
	u.Mu.Unlock()
}

func (u *UpstreamTrace) NoteWSFailure(info codexws.WSInfo, err error) {
	u.Mu.Lock()
	u.Transport = "transport websocket (full: " + SafeWSFallbackReason(err) + ")"
	u.WsConn = "conn new (dial " + RoundDur(info.DialTime) + u.convText() + ")"
	u.Wire.Raw = 0
	u.Wire.Sent = 0
	u.Wire.Encoding = ""
	u.Mu.Unlock()
}

func (u *UpstreamTrace) NoteWebsocket(info codexws.WSInfo, bodyBytes int) {
	frameSize := SizeString(info.SentBytes)
	var transport string
	if info.Mode == "delta" {
		transport = fmt.Sprintf("transport websocket (delta: sent %s of %s)", frameSize, SizeString(bodyBytes))
	} else {
		transport = fmt.Sprintf("transport websocket (full: %s, sent %s)", WsReasonText(info), frameSize)
	}
	conn := "conn new (dial " + RoundDur(info.DialTime) + u.convText() + ")"
	if info.Reused {
		conn = "conn reused (age " + RoundDur(info.ConnAge) + u.convText() + ")"
	}
	if info.Divergence != nil {
		_, _ = fmt.Fprintf(shared.LogOut, "[%s] upstream websocket [codex]%s: %s\n", time.Now().Format("15:04:05"),
			u.convPrefix(), WsDivergenceText(*info.Divergence, u.Prompt))
	}
	u.Mu.Lock()
	u.Transport = transport
	u.WsConn = conn
	u.Wire.Raw = info.SentBytes
	u.Wire.Sent = info.SentBytes
	u.Wire.Encoding = "ws"
	u.Got = time.Time{}
	u.Wrote = time.Time{}
	u.Headers = time.Time{}
	u.Read = time.Time{}
	u.Reused = false
	u.Idle = 0
	u.Warm = false
	u.Tls = 0
	u.Mu.Unlock()
}

func (u *UpstreamTrace) NoteWSFallback(reason string, bodyBytes int) {
	u.Mu.Lock()
	u.HttpNote = u.httpNoteLocked(bodyBytes)
	if u.Wire != nil && u.Wire.Encoding == "ws" {
		u.Wire.Raw = 0
		u.Wire.Sent = 0
		u.Wire.Encoding = ""
	}
	u.Transport = "fallback to HTTP (" + reason + ")"
	u.WsConn = ""
	u.Mu.Unlock()
	CodexWSFallbacks.Add(1)
}

func (u *UpstreamTrace) NoteWSCanceled(err error) {
	u.Mu.Lock()
	u.Transport = "transport websocket (" + SafeWSFallbackReason(err) + ")"
	u.Mu.Unlock()
}
