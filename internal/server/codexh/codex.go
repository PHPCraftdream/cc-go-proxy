package codexh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/catalog"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/sse"
	codexws "github.com/PHPCraftdream/cc-go-proxy/internal/codex/ws"
	wschain "github.com/PHPCraftdream/cc-go-proxy/internal/codex/ws/chain"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/converter"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/codexh/tier"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/codexh/wsh"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/obs"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/rt"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
	"github.com/gofiber/fiber/v2"
)

func init() {
	codex.Notice = func(msg string) {
		_, _ = fmt.Fprintf(shared.LogOut, "[%s] %s\n", time.Now().Format("15:04:05"), msg)
	}
}

// notRetriedNote explains a failed stream that ccgp did not ask for again: the
// client had already got content (sent: its kinds, see Translator.Sent), so the
// request is Claude Code's to resend. After thinking only it does, a few times;
// after text or a tool call it keeps what came and does not.
func notRetriedNote(sent string) string {
	if sent == "" {
		return ""
	}
	return "not retried: content already sent (" + sent + ")"
}

// OverloadBackoff is the pause before each retry of an overloaded backend.
var OverloadBackoff = []time.Duration{2 * time.Second, 5 * time.Second}

// BrokenBackoff is the pause before each retry of a stream whose connection broke
// before it sent any content, or of a request the gateway in front of the
// backend answered with 502/503/504. Short: the next try opens another connection.
var BrokenBackoff = []time.Duration{300 * time.Millisecond, 2 * time.Second}

// noRetry tells Claude Code not to send the request again: a used-up usage
// limit holds for hours, and each retry uploads the whole request.
func noRetry(c *fiber.Ctx) { c.Set("x-should-retry", "false") }

// edgeError reports whether status is a gateway error: the request got no
// further than the edge, so asking again is safe.
func edgeError(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

// callAfter waits, then calls; a non-2xx reply is an error.
func callAfter(ctx context.Context, wait time.Duration, call func() (*http.Response, error)) (*http.Response, error) {
	select {
	case <-time.After(wait):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	resp, err := call()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("retry: HTTP %d: %s", resp.StatusCode, codex.ErrorMessage(b))
	}
	return resp, nil
}

// runUpstream runs tr over the reply. An overloaded backend, or a connection
// that broke, before any content was sent: the request is asked again after a
// pause (again), the client only waits longer. Each kind has its own budget.
// It returns the reply the run ended on; the caller closes it.
func runUpstream(ctx context.Context, tr *sse.Translator, up *http.Response, again func() (*http.Response, error), profile, mapped string, retries ...*wsh.CodexRetry) *http.Response {
	var retry *wsh.CodexRetry
	if len(retries) > 0 {
		retry = retries[0]
	}
	overloads, breaks := 0, 0
	for {
		tr.AllowRetry = overloads < len(OverloadBackoff)
		tr.AllowTransportRetry = breaks < len(BrokenBackoff)
		if err := tr.Run(up.Body); !errors.Is(err, sse.ErrRetry) {
			if err == nil && retry != nil {
				wsh.DrainCompletedWebsocketReply(retry, tr, up)
			}
			return up
		}
		if tr.RetryTransport {
			obs.NoteBreak(tr.Retry, false, true)
		}
		if retry != nil && retry.Websocket {
			_, _ = io.Copy(io.Discard, up.Body)
		}
		var wait time.Duration
		var n, max int
		if tr.RetryTransport {
			wait, max = BrokenBackoff[breaks], len(BrokenBackoff)
			breaks++
			n = breaks
		} else {
			wait, max = OverloadBackoff[overloads], len(OverloadBackoff)
			overloads++
			n = overloads
		}
		rt.LogUpstreamRetry(shared.LogOut, profile, mapped, n, max, wait, tr.Retry, tr.Detail, shared.UpstreamIDs(up.Header))
		tr.Detail = ""
		if tr.Started() {
			tr.Emit("ping", map[string]any{"type": "ping"})
		}
		next, err := callAfter(ctx, wait, again)
		if err != nil {
			tr.Fail(err.Error())
			return up
		}
		_ = up.Body.Close()
		up = next
	}
}

// logUpstreamEnd prints the line for a reply that was cancelled, failed or slow.
// All three are stamped with the start of the call, like the request's own line;
// the summary tells when the reply ended.
func logUpstreamEnd(ctx context.Context, tr *sse.Translator, trace *obs.UpstreamTrace, up *http.Response, profile, mapped string, bodyBytes int) {
	end := time.Now()
	trace.Mu.Lock()
	trace.Edge = obs.EdgeOf(up.Header)
	trace.Mu.Unlock()
	obs.PromptWatch.Done(trace.Fp.Key, trace.PromptGeneration, tr.Usage.Input, trace.Edge)
	obs.PromptStats.Add(tr.Usage, obs.GapGroup(trace.Prev, trace.Start), obs.EdgeGroup(trace.Prev.Edge, trace.Edge), obs.TurnStateGroup(trace.Routing != nil && trace.Routing.Sent()))
	ids := shared.UpstreamIDs(up.Header)
	obs.LinkHealth.Reply(trace.LinkSample())
	if tr.Failed == "" && ctx.Err() == nil && !tr.FirstDelta.IsZero() {
		trace.Mu.Lock()
		kind := obs.SpeedKind(trace.Transport)
		trace.Mu.Unlock()
		obs.ReplySpeed.Add(kind, tr.FirstDelta.Sub(trace.Start), end.Sub(trace.Start))
	}
	if tr.Failed != "" && ctx.Err() == nil {
		obs.NoteBreak(tr.Failed, trace.PingClosed(end), false)
	}
	what := "stream"
	trace.Mu.Lock()
	if trace.NonStream {
		what = "non-stream reply"
	}
	trace.Mu.Unlock()
	switch {
	case tr.Failed != "" && ctx.Err() != nil:
		rt.LogStreamCanceled(shared.LogOut, trace.Start, what, profile, mapped, shared.JoinNonEmpty(trace.Summary(tr, end, bodyBytes), ids))
	case tr.Failed != "":
		shared.LogStreamFailure(shared.LogOut, trace.Start, what, profile, mapped, tr.Failed, tr.Detail,
			shared.JoinNonEmpty(notRetriedNote(tr.Sent()), trace.SilenceText(end), ids, trace.Summary(tr, end, bodyBytes)))
	case trace.Slow(end):
		rt.LogSlowStream(shared.LogOut, trace.Start, what, profile, mapped, shared.JoinNonEmpty(trace.Summary(tr, end, bodyBytes), ids))
	}
}

// codexTurnKind tells where a request stands in its turn from the tail of the
// converted input. A converted user message puts its tool results first and its
// text after them, and Claude Code often appends reminder text to a tool
// result, so trailing user messages are skipped: a tool output reached before
// any assistant item is a continuation, and user messages right after an
// assistant item (or alone) begin a new turn.
func codexTurnKind(req *codex.Request) (continuation, newTurn bool) {
	if req == nil {
		return false, false
	}
	sawUser := false
	for i := len(req.Input) - 1; i >= 0; i-- {
		typ, role, _, _ := wschain.ItemParts(req.Input[i])
		switch {
		case typ == "function_call_output":
			return true, false
		case typ == "message" && role == "user":
			sawUser = true
		default:
			return false, sawUser
		}
	}
	return false, sawUser
}

// codexRateHeaders redacts the backend's opaque turn state before the generic
// rate-header formatter can include it in request logs.
func codexRateHeaders(h http.Header) string {
	filtered := h.Clone()
	for key := range filtered {
		if strings.EqualFold(key, "x-codex-turn-state") {
			delete(filtered, key)
		}
	}
	return shared.RateHeaders(filtered)
}

// handleCodexMessages serves /v1/messages via the ChatGPT Codex backend.
// The upstream call happens before streaming starts so HTTP errors (429, 401)
// reach Claude Code with their real status.
func HandleCodexMessages(c *fiber.Ctx, cfg *config.Config, s *oauth.Session) error {
	body := c.Body()
	if cfg.Debug {
		fmt.Printf("\n=== CLAUDE REQUEST ===\n%s\n=====================\n", body)
	}
	// ParseRequest owns decoded fields; the Fiber-backed body is only read here and for its length below.
	parsed, err := codex.ParseRequest(body)
	if err != nil {
		return shared.AnthropicError(c, fiber.StatusBadRequest, "invalid_request_error", err.Error())
	}
	probe := struct {
		Model string `json:"model"`
	}{Model: parsed.Model()}
	model := converter.MapModel(parsed.Model(), cfg)
	c.Locals(shared.LocalsModel, probe.Model+" -> "+model)

	req, wantStream, err := parsed.Convert(model, cfg.Profile)
	if err != nil {
		return shared.AnthropicError(c, fiber.StatusBadRequest, "invalid_request_error", err.Error())
	}
	baseURL := ""
	if pr := cfg.Providers[cfg.ProviderName]; pr != nil {
		baseURL = pr.BaseURL
	}
	if cfg.Profiles[cfg.Profile].EffectiveFast(catalog.ModelFamily(model), parsed.ClientFast()) {
		var status int
		req.ServiceTier, status, err = tier.FastTier(context.Background(), cfg, baseURL, model, s)
		if err != nil {
			typ := "api_error"
			switch status {
			case fiber.StatusUnauthorized:
				typ = "authentication_error"
			case fiber.StatusBadRequest:
				typ = "invalid_request_error"
			}
			return shared.AnthropicError(c, status, typ, err.Error())
		}
	}
	if cfg.Debug {
		b, _ := json.Marshal(req)
		fmt.Printf("\n=== CODEX REQUEST (%s → %s) ===\n%s\n", probe.Model, model, b)
	}
	digest := codex.NewDigest(req)
	fingerprint := digest.Fingerprint
	continuation, newTurn := codexTurnKind(req)
	turnRegistration := obs.TurnStates.Begin(fingerprint, continuation, newTurn, time.Now())
	routing := codex.NewRouting(turnRegistration.ThreadID, turnRegistration.State, func(state string) {
		turnRegistration.Store(state, time.Now())
	})
	ctx, cancel := context.WithCancel(rt.UpstreamCtx(context.Background(), cfg))
	wsCtx := codex.WithDigest(codex.WithRouting(ctx, routing), digest)
	trace := obs.NewUpstreamTrace()
	trace.NonStream = !wantStream
	if !wantStream {
		// Nothing reaches the client before the reply is complete, so its going
		// away shows on its connection, not in a failed write.
		stop := rt.WatchClientGone(c.Context().Conn(), cancel)
		defer stop()
		c.Context().SetConnectionClose()
	}
	trace.Routing = routing
	trace.NewTurn = turnRegistration.NewTurn
	trace.Fp = fingerprint
	registration := obs.PromptWatch.See(trace.Fp, digest.Hashes())
	trace.Prev = registration.Prev
	trace.PromptGeneration = registration.Generation
	originalCancel := cancel
	cancel = func() {
		obs.PromptWatch.Finish(trace.Fp.Key, trace.PromptGeneration)
		originalCancel()
	}
	trace.Prompt = obs.PromptText(trace.Fp, registration, time.Now())
	trace.OnRetry = trace.ReconnectHook(cfg.Profile, probe.Model+" -> "+model)
	trace.Wire = &codex.Wire{}
	connObs := trace.Observer()
	ctx = connpool.WithObserver(httptrace.WithClientTrace(codex.WithRouting(codex.WithWire(ctx, trace.Wire), routing), trace.ClientTrace()), connObs)
	retry := &wsh.CodexRetry{
		Call: func(p *codex.Prepared) (*http.Response, error) {
			return codex.CallPrepared(ctx, baseURL, s, p)
		},
	}
	var resp *http.Response
	requestBytes := len(body)
	// A background request of Claude Code would replace the conversation's
	// websocket chain with its own reply: it goes over HTTP (wsside.go).
	useWS := cfg.CodexWebsocket() && fingerprint.Key != ""
	// One websocket per agent: the subagents of a conversation share its
	// thread (key, system prompt, tools) and would replace each other's chain.
	bucket := ""
	if useWS {
		agent := wschain.AgentKey(req)
		trace.Conv = wsh.WsConvTag(turnRegistration.ThreadID) + wsh.WsAgentTag(agent)
		bucket = turnRegistration.ThreadID + ":" + agent
		side, wrong := wsh.CurrentCodexWSManager().SideRequest(bucket, req)
		if wrong != nil {
			wsh.LogWSSideWrong(trace.Conv, *wrong)
		}
		if side {
			useWS = false
			trace.NoteSideRequest(requestBytes)
		}
	}
	if useWS {
		retry.Request = req
		retry.Websocket = true
		retry.CallWS = func(request *codex.Request) (*http.Response, codexws.WSInfo, error) {
			return wsh.CurrentCodexWSManager().Call(wsCtx, baseURL, s, bucket, request)
		}
		resp, err = retry.RequestCall(ctx, trace, requestBytes)
	} else {
		prepared, prepareErr := codex.PrepareRequest(req)
		retry.Prepared = prepared
		err = prepareErr
		if err == nil {
			resp, err = retry.CallPrepared()
		}
	}
	// A short rate limit is waited out here, once; a long one is Claude Code's to handle.
	if err == nil && resp.StatusCode == http.StatusTooManyRequests && !retry.Websocket {
		if wait, ok := shared.RetryAfter(resp.Header, time.Now()); ok && wait <= shared.RateRetryMax {
			shared.LogRateRetry(shared.LogOut, cfg.Profile, probe.Model+" -> "+model, wait, codexRateHeaders(resp.Header))
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			if shared.SleepCtx(ctx, wait) {
				resp, err = retry.CallPrepared()
			} else {
				err = ctx.Err()
			}
		}
	}
	// A gateway error, or an HTML page the edge turned the connection away with
	// (a block of its route), comes from the edge in front of the backend, not the
	// backend: it is asked again after a short pause, on a fresh connection after
	// a page. Nothing ran yet, and the client would only send the same request itself.
	for try := 0; err == nil && !retry.Websocket && try < len(BrokenBackoff); try++ {
		status := resp.StatusCode
		if !edgeError(status) && status != http.StatusForbidden {
			break
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		blocked := status == http.StatusForbidden && codex.IsHTMLPage(b)
		if status == http.StatusForbidden && !blocked {
			resp.Body = io.NopCloser(bytes.NewReader(b)) // the backend's own 403: the client gets it
			break
		}
		if blocked {
			connObs.Discard()
		}
		rt.LogUpstreamRetry(shared.LogOut, cfg.Profile, probe.Model+" -> "+model, try+1, len(BrokenBackoff), BrokenBackoff[try],
			fmt.Sprintf("HTTP %d: %s", status, codex.ErrorMessage(b)), "", shared.JoinNonEmpty(shared.UpstreamIDs(resp.Header), trace.ConnText()))
		if !shared.SleepCtx(ctx, BrokenBackoff[try]) {
			err = ctx.Err()
			break
		}
		resp, err = retry.CallPrepared()
	}
	if err != nil {
		// Canceled before ccgp's own cancel: the client of a non-stream request
		// left (watchClientGone) before the backend's reply started.
		left := ctx.Err() != nil
		cancel()
		if retry.Websocket && ctx.Err() != nil {
			trace.NoteWSCanceled(ctx.Err())
		}
		if left {
			what := "stream"
			if !wantStream {
				what = "non-stream reply"
			}
			rt.LogStreamCanceled(shared.LogOut, trace.Start, what, cfg.Profile, probe.Model+" -> "+model,
				shared.JoinNonEmpty("before the reply started", trace.ConnText()))
		}
		if errors.Is(err, oauth.ErrNotLoggedIn) || errors.Is(err, oauth.ErrReloginRequired) {
			return shared.AnthropicError(c, fiber.StatusUnauthorized, "authentication_error",
				fmt.Sprintf("%v (ccgp --login %s)", err, cfg.ProviderName))
		}
		out := shared.AnthropicError(c, fiber.StatusBadGateway, "api_error", err.Error())
		// The log gets the hint and how far the request got; the client the message.
		now := time.Now()
		msg := err.Error()
		obs.NoteBreak(msg, trace.PingHint(err, now) != "", false)
		if hint := trace.PingHint(err, now); hint != "" {
			msg += " " + hint
		}
		if sent := trace.SentText(now); sent != "" {
			msg += " | " + sent
		}
		c.Locals(shared.LocalsErr, msg)
		c.Locals(shared.LocalsConn, trace.ConnText())
		return out
	}
	c.Locals(shared.LocalsConn, trace.NewConnText())
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		cancel()
		c.Locals(shared.LocalsConn, trace.ConnText())
		status := resp.StatusCode
		if status == http.StatusUnauthorized {
			// Still 401 after refresh: credentials are no longer accepted.
			return shared.AnthropicError(c, status, "authentication_error",
				fmt.Sprintf("codex: unauthorized, run `ccgp --login %s`: %s", cfg.ProviderName, codex.ErrorMessage(b)))
		}
		msg := "codex: " + codex.ErrorMessage(b)
		if required, ok := codex.NeedsNewerClient(status, b); ok {
			note, raised := tier.RaiseClientVersion(cfg, s, required)
			msg += " | " + note
			if raised {
				// Claude Code retries a 503; the retry goes out with the new version.
				c.Locals(shared.LocalsErr, msg)
				return shared.AnthropicError(c, fiber.StatusServiceUnavailable, "api_error", msg)
			}
		}
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			c.Set("Retry-After", ra) // Claude Code backs off by it
		}
		if status == http.StatusTooManyRequests && codex.IsUsageLimit(b) {
			noRetry(c)
		}
		hdrs := codexRateHeaders(resp.Header)
		out := shared.AnthropicError(c, status, codex.ErrorType(status), msg)
		// The log gets the limits and the ids; the client the message.
		if status != http.StatusTooManyRequests {
			hdrs = ""
		}
		if trail := shared.JoinNonEmpty(hdrs, shared.UpstreamIDs(resp.Header)); trail != "" {
			c.Locals(shared.LocalsErr, msg+" | "+trail)
		}
		return out
	}

	// The reply is read to its end here, so a failed one can be asked again
	// (see runUpstream) as well as in a stream.
	resp.Body = trace.Stamp(resp.Body)
	again := func() (*http.Response, error) { return retry.CallAgain(ctx, trace, requestBytes) }
	profile, mapped, bodyBytes := cfg.Profile, probe.Model+" -> "+model, len(body)

	if !wantStream {
		defer cancel()
		col := &sse.Collector{}
		tr := &sse.Translator{Model: probe.Model, Emit: retry.Emit(col.Emit)}
		upstream := runUpstream(ctx, tr, resp, again, profile, mapped, retry)
		defer func() { _ = upstream.Body.Close() }()
		logUpstreamEnd(ctx, tr, trace, upstream, profile, mapped, bodyBytes)
		out, err := col.Result()
		if err != nil && tr.FailedCode == codex.UsageLimitCode {
			noRetry(c)
			return shared.AnthropicError(c, fiber.StatusTooManyRequests, "rate_limit_error", err.Error())
		}
		if err != nil {
			return shared.AnthropicError(c, fiber.StatusBadGateway, "api_error", err.Error())
		}
		return c.JSON(out)
	}

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")
	// The access line is printed when the handler returns, before the stream
	// ends; a failure inside the stream gets its own line.
	release := rt.DeferActive(c)
	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		defer release()
		defer cancel()
		tr := &sse.Translator{Model: probe.Model, Emit: retry.Emit(func(ev string, data any) {
			if err := rt.WriteSSEEvent(w, ev, data); err != nil {
				cancel() // client gone: abort upstream
				return
			}
			if err := w.Flush(); err != nil {
				cancel() // client gone: abort upstream
			}
		})}
		upstream := runUpstream(ctx, tr, resp, again, profile, mapped, retry)
		defer func() { _ = upstream.Body.Close() }()
		logUpstreamEnd(ctx, tr, trace, upstream, profile, mapped, bodyBytes)
	})
	return nil
}
