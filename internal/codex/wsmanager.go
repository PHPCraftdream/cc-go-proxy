package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

var (
	ErrWSBusy        = errWSBusy
	ErrWSUnavailable = errors.New("codex websocket unavailable")
	// ErrWSToken: no usable login (logged out, refresh failed); HTTP reports it.
	ErrWSToken = errors.New("codex websocket: no usable token")
)

var (
	wsManagerMaxConnections = 8
	// wsManagerMaxAge is the planned age of a connection: the backend ends them
	// after a fixed age (30 minutes seen), so they are replaced before it.
	wsManagerMaxAge       = 25 * time.Minute
	wsManagerMaxIdle      = 10 * time.Minute
	wsManagerJanitorEvery = time.Minute
	// wsManagerDisabledFor: the endpoint said it has no websocket (400, 404, 405, 410, 426, 501).
	wsManagerDisabledFor = 10 * time.Minute
	// wsManagerBackoffFor: any other failed dial (401, 403, network, TLS, timeout):
	// not proof of "unsupported", only a pause so each request does not pay for it.
	wsManagerBackoffFor = 30 * time.Second
)

// wsUnsupported tells handshake statuses that mean "no websocket here".
func wsUnsupported(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusGone,
		http.StatusUpgradeRequired, http.StatusNotImplemented:
		return true
	}
	return false
}

// wsOwnerKey is who a connection belongs to: endpoint, account and way out.
// Kept in memory only (the proxy key may hold a password).
func wsOwnerKey(baseURL, account string, choice proxydial.Choice) string {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return baseURL + "\x00" + account + "\x00" + choice.Key()
}

// wsAccount names the login: its account id, else its token file.
func wsAccount(s *oauth.Session, tok *oauth.Tokens) string {
	if tok != nil && tok.AccountID != "" {
		return "account:" + tok.AccountID
	}
	return "file:" + s.Store.Path
}

// WSInfo says how a request went out.
type WSInfo struct {
	Mode      string
	SentBytes int
	Reused    bool
	Reason    string
	ConnAge   time.Duration
	DialTime  time.Duration
	// Limit is the lowered planned age (0: the default), set when Reason says
	// a connection was replaced at it.
	Limit time.Duration
}

// WSStats counts the exchanges of a manager.
type WSStats struct {
	Delta     int
	Full      int
	Busy      int
	Fallbacks int
	Dials     int
	SentBytes int64
	// Lost counts connections the peer or the network ended (we did not close
	// them); Early those replaced ahead of the learned limit.
	Lost  int
	Early int
}

type wsManagerEntry struct {
	key        string // owner + bucket: the map key
	owner      string
	session    *oauth.Session
	account    string
	bucket     string
	conn       *WSConn
	chain      *WSChain
	dialedAt   time.Time
	lastUse    time.Time
	busy       bool
	generation uint64
}

// WSManager keeps one websocket per conversation bucket and the chain state that
// lets a later request be sent as a delta.
type WSManager struct {
	mu        sync.Mutex
	entries   map[string]*wsManagerEntry
	stats     WSStats
	closing   int
	lifetimes map[string]*wsLifetime
	// OnLimit is called (without the manager's lock) when a connection age limit
	// is learned or changes.
	OnLimit      func(WSLimit)
	now          func() time.Time
	maxEntries   int
	maxAge       time.Duration
	maxIdle      time.Duration
	janitorEvery time.Duration
	disabledFor  time.Duration
	backoffFor   time.Duration
	ctx          context.Context
	cancel       context.CancelFunc
	janitorDone  chan struct{}
	janitorOn    bool
	closeDone    chan struct{}
	closed       bool
	dials        sync.WaitGroup
	disabled     map[string]time.Time // owner -> no websocket until
}

// NewWSManager returns an empty manager; the zero value works as well.
func NewWSManager() *WSManager { return &WSManager{} }

func (m *WSManager) initLocked() {
	if m.entries == nil {
		m.entries = make(map[string]*wsManagerEntry)
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.maxEntries <= 0 {
		m.maxEntries = wsManagerMaxConnections
	}
	if m.maxEntries < 1 {
		m.maxEntries = 8
	}
	if m.maxAge <= 0 {
		m.maxAge = wsManagerMaxAge
	}
	if m.maxAge <= 0 {
		m.maxAge = 25 * time.Minute
	}
	if m.maxIdle <= 0 {
		m.maxIdle = wsManagerMaxIdle
	}
	if m.maxIdle <= 0 {
		m.maxIdle = 10 * time.Minute
	}
	if m.janitorEvery <= 0 {
		m.janitorEvery = wsManagerJanitorEvery
	}
	if m.janitorEvery <= 0 {
		m.janitorEvery = time.Minute
	}
	if m.disabledFor <= 0 {
		m.disabledFor = wsManagerDisabledFor
	}
	if m.disabledFor <= 0 {
		m.disabledFor = 10 * time.Minute
	}
	if m.backoffFor <= 0 {
		m.backoffFor = wsManagerBackoffFor
	}
	if m.backoffFor <= 0 {
		m.backoffFor = 30 * time.Second
	}
	if m.disabled == nil {
		m.disabled = make(map[string]time.Time)
	}
	if m.lifetimes == nil {
		m.lifetimes = make(map[string]*wsLifetime)
	}
	if m.ctx == nil {
		m.ctx, m.cancel = context.WithCancel(context.Background())
	}
	if m.closeDone == nil {
		m.closeDone = make(chan struct{})
	}
}

func (m *WSManager) startJanitorLocked() {
	if m.janitorOn || m.closed {
		return
	}
	m.janitorOn = true
	m.janitorDone = make(chan struct{})
	go m.janitor(m.ctx, m.janitorDone, m.janitorEvery)
}

// Close closes every connection and stops the janitor; later calls get ErrWSUnavailable.
func (m *WSManager) Close() {
	m.mu.Lock()
	m.initLocked()
	if m.closed {
		done := m.closeDone
		m.mu.Unlock()
		<-done
		return
	}
	m.closed = true
	m.cancel()
	connections := make([]*WSConn, 0, len(m.entries))
	for _, entry := range m.entries {
		if entry.conn != nil {
			entry.generation++
			connections = append(connections, entry.conn)
		}
	}
	m.entries = make(map[string]*wsManagerEntry)
	janitorDone := m.janitorDone
	closeDone := m.closeDone
	m.mu.Unlock()

	for _, conn := range connections {
		_ = conn.Close()
	}
	m.dials.Wait()
	if janitorDone != nil {
		<-janitorDone
	}
	close(closeDone)
}

// Call sends req on the bucket's websocket: as an increment of the previous
// response when the backend still holds it, else in full. The reply is an SSE
// stream like the HTTP one. ErrWSBusy and ErrWSUnavailable mean: use HTTP.
func (m *WSManager) Call(ctx context.Context, baseURL string, s *oauth.Session, bucket string, req *Request) (*http.Response, WSInfo, error) {
	if ctx == nil {
		return nil, WSInfo{}, errors.New("codex websocket: invalid context")
	}
	if err := ctx.Err(); err != nil {
		return nil, WSInfo{}, err
	}
	if req == nil {
		return nil, WSInfo{}, errors.New("codex websocket: nil request")
	}
	if s == nil {
		return nil, WSInfo{}, fmt.Errorf("%w: %w", ErrWSUnavailable, ErrWSToken)
	}
	// The login is checked on every use, as the HTTP path does: after a logout or
	// a failed refresh no connection of this session is used (they are closed),
	// and a new account never gets a connection of the old one.
	tok, err := s.Token(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, WSInfo{}, ctx.Err()
		}
		m.closeSessionExcept(s, "")
		return nil, WSInfo{}, fmt.Errorf("%w: %w", ErrWSUnavailable, ErrWSToken)
	}
	account := wsAccount(s, tok)
	m.closeSessionExcept(s, account)
	owner := wsOwnerKey(baseURL, account, proxydial.ChoiceFor(ctx))
	key := owner + "\x00" + bucket

	m.mu.Lock()
	m.initLocked()
	if m.closed {
		m.mu.Unlock()
		return nil, WSInfo{}, ErrWSUnavailable
	}
	if m.disabledLocked(owner) {
		m.mu.Unlock()
		return nil, WSInfo{}, ErrWSUnavailable
	}
	m.startJanitorLocked()
	m.mu.Unlock()

	var entry *wsManagerEntry
	var conn *WSConn
	var chain *WSChain
	var reused bool
	var connAge time.Duration
	var reason string
	var needsDial bool
	var generation uint64
	var evicted *WSConn
	var lost *WSConn
	var learnedLimit time.Duration

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, WSInfo{}, ErrWSUnavailable
	}
	if m.disabledLocked(owner) {
		m.mu.Unlock()
		return nil, WSInfo{}, ErrWSUnavailable
	}
	now := m.now()
	entry = m.entries[key]
	if entry != nil && entry.busy {
		m.stats.Busy++
		m.mu.Unlock()
		return nil, WSInfo{}, ErrWSBusy
	}
	if entry != nil && entry.conn != nil {
		limit, lowered := m.ageLimitLocked(owner, now)
		switch {
		case entry.conn.Err() != nil:
			reason = "connection broken"
			lost = entry.conn
		case now.Sub(entry.dialedAt) > limit:
			reason = "connection too old"
			if lowered {
				reason = "lowered age limit"
				learnedLimit = limit
				m.stats.Early++
			}
		case now.Sub(entry.lastUse) > m.maxIdle:
			reason = "connection too old"
		default:
			conn = entry.conn
			chain = entry.chain
			reused = true
			connAge = nonnegativeDuration(now.Sub(entry.dialedAt))
		}
	}
	if conn == nil {
		if entry != nil && entry.conn != nil {
			evicted = entry.conn
			m.closing++
			entry.conn = nil
			entry.chain = nil
			entry.generation++
		}
		if entry == nil {
			if len(m.entries) >= m.maxEntries {
				if m.closing > 0 {
					m.stats.Busy++
					m.mu.Unlock()
					return nil, WSInfo{}, ErrWSBusy
				}
				var oldest *wsManagerEntry
				for _, candidate := range m.entries {
					if candidate.busy {
						continue
					}
					if oldest == nil || candidate.lastUse.Before(oldest.lastUse) {
						oldest = candidate
					}
				}
				if oldest == nil {
					m.stats.Busy++
					m.mu.Unlock()
					if evicted != nil {
						_ = evicted.Close()
					}
					return nil, WSInfo{}, ErrWSBusy
				}
				delete(m.entries, oldest.key)
				oldest.generation++
				evicted = oldest.conn
				if evicted != nil {
					m.closing++
				}
			} else if len(m.entries)+m.closing >= m.maxEntries {
				m.stats.Busy++
				m.mu.Unlock()
				if evicted != nil {
					m.closeTracked(evicted)
				}
				return nil, WSInfo{}, ErrWSBusy
			}
			entry = &wsManagerEntry{key: key, owner: owner, session: s, account: account, bucket: bucket}
			m.entries[key] = entry
		}
		entry.busy = true
		entry.generation++
		generation = entry.generation
		needsDial = true
		m.stats.Dials++
		m.dials.Add(1)
	} else {
		entry.busy = true
		entry.generation++
		generation = entry.generation
	}
	m.mu.Unlock()
	if evicted != nil {
		m.closeTracked(evicted)
	}
	m.noteLost(owner, lost)

	snapshot := req // not modified by the caller while the exchange runs; the chain keeps only hashes
	if needsDial {
		dialStarted := m.now()
		dialCtx, cancel := context.WithCancel(ctx)
		stopManagerCancel := context.AfterFunc(m.ctx, cancel)
		newConn, dialErr := DialWS(dialCtx, baseURL, s, WSHandshake{Request: snapshot, Routing: routingFrom(ctx)})
		dialTime := nonnegativeDuration(m.now().Sub(dialStarted))
		stopManagerCancel()
		cancel()
		if dialErr != nil {
			dialErr = m.handleDialError(ctx, owner, dialErr)
			m.failReservation(entry, generation)
			m.dials.Done()
			return nil, WSInfo{Mode: "full", Reason: reason, DialTime: dialTime}, dialErr
		}
		m.mu.Lock()
		if m.closed || m.entries[key] != entry || !entry.busy || entry.generation != generation {
			m.mu.Unlock()
			_ = newConn.Close()
			m.dials.Done()
			if ctx.Err() != nil {
				return nil, WSInfo{Mode: "full", DialTime: dialTime}, ctx.Err()
			}
			return nil, WSInfo{Mode: "full", DialTime: dialTime}, ErrWSUnavailable
		}
		conn = newConn
		entry.conn = conn
		entry.chain = nil
		entry.dialedAt = m.now()
		entry.lastUse = entry.dialedAt
		m.mu.Unlock()
		m.dials.Done()
		if reason == "" {
			reason = "new connection"
		}
		info := WSInfo{Mode: "full", Reused: false, Reason: reason, ConnAge: connAge, DialTime: dialTime, Limit: learnedLimit}
		return m.send(ctx, entry, generation, conn, nil, snapshot, info)
	}

	info := WSInfo{Mode: "full", Reused: reused, Reason: reason, ConnAge: connAge}
	return m.send(ctx, entry, generation, conn, chain, snapshot, info)
}

func (m *WSManager) send(ctx context.Context, entry *wsManagerEntry, generation uint64, conn *WSConn, chain *WSChain, req *Request, info WSInfo) (*http.Response, WSInfo, error) {
	var frame []byte
	fallback := false
	if chain != nil || info.Reused {
		if delta, ok, why := chain.Delta(req); ok {
			var err error
			frame, err = WSCreatePayload(req, chain.ResponseID(), delta)
			if err != nil {
				m.releaseWithoutExchange(entry, generation, conn)
				return nil, info, err
			}
			info.Mode = "delta"
		} else {
			info.Reason = why
			fallback = true
		}
	}
	if fallback {
		m.mu.Lock()
		m.stats.Fallbacks++
		m.mu.Unlock()
	}
	if frame == nil {
		var err error
		frame, err = WSCreatePayload(req, "", req.Input)
		if err != nil {
			m.releaseWithoutExchange(entry, generation, conn)
			return nil, info, err
		}
	}
	resp, err := conn.Do(ctx, frame)
	if err != nil {
		m.dropLease(entry, generation, conn)
		return nil, info, err
	}
	info.SentBytes = len(frame)
	m.mu.Lock()
	if info.Mode == "delta" {
		m.stats.Delta++
	} else {
		m.stats.Full++
	}
	m.stats.SentBytes += int64(len(frame))
	m.mu.Unlock()
	resp.Body = &wsManagerBody{
		body:       resp.Body,
		manager:    m,
		entry:      entry,
		generation: generation,
		conn:       conn,
		req:        req,
		result:     conn.Result(),
	}
	return resp, info, nil
}

func (m *WSManager) closeTracked(conn *WSConn) {
	if conn == nil {
		return
	}
	_ = conn.Close()
	m.mu.Lock()
	m.closing--
	m.mu.Unlock()
}

// handleDialError decides how long owner goes without a websocket after a failed
// dial: disabledFor when the endpoint said it has none, nothing for 429 and 5xx
// (passing trouble; this request still goes over HTTP), backoffFor otherwise.
// A canceled request says nothing about the endpoint.
func (m *WSManager) handleDialError(ctx context.Context, owner string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var handshake *WSHandshakeError
	status := 0
	if errors.As(err, &handshake) {
		status = handshake.Status
	}
	if status == http.StatusTooManyRequests || status >= 500 && !wsUnsupported(status) {
		return err
	}
	pause := m.backoffFor
	if wsUnsupported(status) {
		pause = m.disabledFor
	}
	if !m.closed {
		m.disabled[owner] = m.now().Add(pause)
	}
	return fmt.Errorf("%w: %w", ErrWSUnavailable, err)
}

// disabledLocked reports that owner is in a no-websocket window; expired
// windows are dropped.
func (m *WSManager) disabledLocked(owner string) bool {
	until, ok := m.disabled[owner]
	if !ok {
		return false
	}
	if m.now().Before(until) {
		return true
	}
	delete(m.disabled, owner)
	return false
}

// closeSessionExcept closes the idle connections of s that belong to another
// account than account ("" = every one of them): after a logout or an account
// switch no old connection is left open. Busy ones end with their exchange.
func (m *WSManager) closeSessionExcept(s *oauth.Session, account string) {
	var closeList []*WSConn
	m.mu.Lock()
	for key, entry := range m.entries {
		if entry.session != s || entry.account == account || entry.busy {
			continue
		}
		delete(m.entries, key)
		entry.generation++
		entry.chain = nil
		if entry.conn != nil {
			m.closing++
			closeList = append(closeList, entry.conn)
		}
	}
	m.mu.Unlock()
	for _, conn := range closeList {
		m.closeTracked(conn)
	}
}

func (m *WSManager) failReservation(entry *wsManagerEntry, generation uint64) {
	m.mu.Lock()
	if m.entries[entry.key] == entry && entry.busy && entry.generation == generation {
		delete(m.entries, entry.key)
	}
	m.mu.Unlock()
}

func (m *WSManager) releaseWithoutExchange(entry *wsManagerEntry, generation uint64, conn *WSConn) {
	m.mu.Lock()
	if !m.closed && m.entries[entry.key] == entry && entry.busy && entry.generation == generation && entry.conn == conn {
		entry.busy = false
		entry.lastUse = m.now()
	}
	m.mu.Unlock()
}

func (m *WSManager) dropLease(entry *wsManagerEntry, generation uint64, conn *WSConn) {
	m.noteLost(entry.owner, conn)
	m.mu.Lock()
	removed := m.entries[entry.key] == entry && entry.busy && entry.generation == generation && entry.conn == conn
	if removed {
		delete(m.entries, entry.key)
		entry.generation++
		m.closing++
	}
	m.mu.Unlock()
	if removed {
		m.closeTracked(conn)
	} else {
		_ = conn.Close()
	}
}

func (m *WSManager) finishLease(entry *wsManagerEntry, generation uint64, conn *WSConn, req *Request, result WSResult, retain bool) {
	m.noteLost(entry.owner, conn)
	var chain *WSChain
	if retain {
		retain = result.Completed && conn.Err() == nil
		if retain {
			chain = NewChain(req, result.ResponseID, result.Items)
		}
	}
	m.mu.Lock()
	if m.entries[entry.key] != entry || !entry.busy || entry.generation != generation || entry.conn != conn {
		m.mu.Unlock()
		return
	}
	if retain && !m.closed {
		entry.chain = chain
		entry.busy = false
		entry.lastUse = m.now()
		m.mu.Unlock()
		return
	}
	delete(m.entries, entry.key)
	m.closing++
	m.mu.Unlock()
	m.closeTracked(conn)
}

// TakeStats returns the counts since the last call and starts them again.
func (m *WSManager) TakeStats() WSStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initLocked()
	stats := m.stats
	m.stats = WSStats{}
	return stats
}

func (m *WSManager) janitor(ctx context.Context, done chan struct{}, interval time.Duration) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.cleanIdle()
		}
	}
}

func (m *WSManager) cleanIdle() {
	var closeList, lostList []*WSConn
	var lostOwners []string
	m.mu.Lock()
	now := m.now()
	for key, entry := range m.entries {
		if entry.conn == nil {
			continue
		}
		dead := entry.conn.Err() != nil
		if !dead && entry.busy {
			continue
		}
		limit, _ := m.ageLimitLocked(entry.owner, now)
		if dead || now.Sub(entry.lastUse) > m.maxIdle || now.Sub(entry.dialedAt) > limit {
			if dead {
				lostList = append(lostList, entry.conn)
				lostOwners = append(lostOwners, entry.owner)
			}
			delete(m.entries, key)
			entry.generation++
			entry.chain = nil
			m.closing++
			closeList = append(closeList, entry.conn)
		}
	}
	m.mu.Unlock()
	for i, conn := range lostList {
		m.noteLost(lostOwners[i], conn)
	}
	for _, conn := range closeList {
		m.closeTracked(conn)
	}
}

func nonnegativeDuration(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

type wsManagerBody struct {
	mu         sync.Mutex
	body       io.ReadCloser
	manager    *WSManager
	entry      *wsManagerEntry
	generation uint64
	conn       *WSConn
	req        *Request
	result     WSResult
	eof        bool
	closed     bool
	finishOnce sync.Once
}

func (b *wsManagerBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return b.body.Read(p)
	}
	n, err := b.body.Read(p)
	var result WSResult
	if err == io.EOF {
		result = b.conn.Result()
	}
	b.mu.Lock()
	if result.Completed {
		b.result = result
	}
	if err == io.EOF && !b.closed {
		b.eof = true
	}
	eof := b.eof
	finalResult := b.result
	b.mu.Unlock()
	if err == io.EOF {
		b.finishOnce.Do(func() { b.manager.finishLease(b.entry, b.generation, b.conn, b.req, finalResult, eof) })
	} else if err != nil {
		b.finishOnce.Do(func() { b.manager.finishLease(b.entry, b.generation, b.conn, b.req, finalResult, false) })
	}
	return n, err
}

func (b *wsManagerBody) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	eof := b.eof
	result := b.result
	b.mu.Unlock()
	err := b.body.Close()
	b.finishOnce.Do(func() { b.manager.finishLease(b.entry, b.generation, b.conn, b.req, result, eof) })
	return err
}

var _ io.ReadCloser = (*wsManagerBody)(nil)

// ageLimitLocked is the age a connection of owner is replaced at: the default,
// or the lowered one (lowered = true then).
func (m *WSManager) ageLimitLocked(owner string, now time.Time) (limit time.Duration, lowered bool) {
	if l := m.lifetimes[owner]; l != nil {
		if cur := l.current(m.maxAge, now); cur < m.maxAge {
			return cur, true
		}
	}
	return m.maxAge, false
}

// noteLost counts a connection the peer ended (once per connection) and lowers
// the planned age when they end early, again and again.
func (m *WSManager) noteLost(owner string, conn *WSConn) {
	age, status, ok := conn.TakePeerLost()
	if !ok {
		return
	}
	m.observeLost(owner, age, status)
}

// observeLost takes a break of a connection of the given age.
func (m *WSManager) observeLost(owner string, age time.Duration, status int) {
	m.mu.Lock()
	m.initLocked()
	m.stats.Lost++
	l := m.lifetimes[owner]
	if l == nil {
		l = &wsLifetime{}
		m.lifetimes[owner] = l
	}
	was := l.current(m.maxAge, m.now())
	limit, lowered := l.observe(m.maxAge, age, m.now())
	notify := m.OnLimit
	m.mu.Unlock()
	if lowered && notify != nil {
		notify(WSLimit{Age: limit, Was: was, Broke: age, Status: status})
	}
}
