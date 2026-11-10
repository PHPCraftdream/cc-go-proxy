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
)

var (
	ErrWSBusy        = errWSBusy
	ErrWSUnavailable = errors.New("codex websocket unavailable")
)

var (
	wsManagerMaxConnections = 8
	wsManagerMaxAge         = 55 * time.Minute
	wsManagerMaxIdle        = 10 * time.Minute
	wsManagerJanitorEvery   = time.Minute
	wsManagerDisabledFor    = 10 * time.Minute
)

// WSInfo says how a request went out.
type WSInfo struct {
	Mode      string
	SentBytes int
	Reused    bool
	Reason    string
	ConnAge   time.Duration
	DialTime  time.Duration
}

// WSStats counts the exchanges of a manager.
type WSStats struct {
	Delta     int
	Full      int
	Busy      int
	Fallbacks int
	Dials     int
	SentBytes int64
}

type wsManagerEntry struct {
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
	mu           sync.Mutex
	entries      map[string]*wsManagerEntry
	stats        WSStats
	closing      int
	now          func() time.Time
	maxEntries   int
	maxAge       time.Duration
	maxIdle      time.Duration
	janitorEvery time.Duration
	disabledFor  time.Duration
	ctx          context.Context
	cancel       context.CancelFunc
	janitorDone  chan struct{}
	janitorOn    bool
	closeDone    chan struct{}
	closed       bool
	dials        sync.WaitGroup
	disabledTil  time.Time
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
		m.maxAge = 55 * time.Minute
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

	m.mu.Lock()
	m.initLocked()
	if m.closed {
		m.mu.Unlock()
		return nil, WSInfo{}, ErrWSUnavailable
	}
	if !m.disabledTil.IsZero() && m.now().Before(m.disabledTil) {
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

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, WSInfo{}, ErrWSUnavailable
	}
	if !m.disabledTil.IsZero() && m.now().Before(m.disabledTil) {
		m.mu.Unlock()
		return nil, WSInfo{}, ErrWSUnavailable
	}
	now := m.now()
	entry = m.entries[bucket]
	if entry != nil && entry.busy {
		m.stats.Busy++
		m.mu.Unlock()
		return nil, WSInfo{}, ErrWSBusy
	}
	if entry != nil && entry.conn != nil {
		switch {
		case entry.conn.Err() != nil:
			reason = "connection broken"
		case now.Sub(entry.dialedAt) > m.maxAge:
			reason = "connection too old"
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
				delete(m.entries, oldest.bucket)
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
			entry = &wsManagerEntry{bucket: bucket}
			m.entries[bucket] = entry
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
			dialErr = m.handleDialError(ctx, dialErr)
			m.failReservation(entry, generation)
			m.dials.Done()
			return nil, WSInfo{Mode: "full", Reason: reason, DialTime: dialTime}, dialErr
		}
		m.mu.Lock()
		if m.closed || m.entries[bucket] != entry || !entry.busy || entry.generation != generation {
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
		info := WSInfo{Mode: "full", Reused: false, Reason: reason, ConnAge: connAge, DialTime: dialTime}
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

func (m *WSManager) handleDialError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	m.mu.Lock()
	if ctx.Err() != nil {
		cancelErr := ctx.Err()
		m.mu.Unlock()
		return cancelErr
	}
	var handshake *WSHandshakeError
	if errors.As(err, &handshake) && (handshake.Status == http.StatusTooManyRequests || handshake.Status >= 500) {
		m.mu.Unlock()
		return err
	}
	if !m.closed {
		m.disabledTil = m.now().Add(m.disabledFor)
	}
	m.mu.Unlock()
	return fmt.Errorf("%w: %v", ErrWSUnavailable, err)
}

func (m *WSManager) failReservation(entry *wsManagerEntry, generation uint64) {
	m.mu.Lock()
	if m.entries[entry.bucket] == entry && entry.busy && entry.generation == generation {
		delete(m.entries, entry.bucket)
	}
	m.mu.Unlock()
}

func (m *WSManager) releaseWithoutExchange(entry *wsManagerEntry, generation uint64, conn *WSConn) {
	m.mu.Lock()
	if !m.closed && m.entries[entry.bucket] == entry && entry.busy && entry.generation == generation && entry.conn == conn {
		entry.busy = false
		entry.lastUse = m.now()
	}
	m.mu.Unlock()
}

func (m *WSManager) dropLease(entry *wsManagerEntry, generation uint64, conn *WSConn) {
	m.mu.Lock()
	removed := m.entries[entry.bucket] == entry && entry.busy && entry.generation == generation && entry.conn == conn
	if removed {
		delete(m.entries, entry.bucket)
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
	var chain *WSChain
	if retain {
		retain = result.Completed && conn.Err() == nil
		if retain {
			chain = NewChain(req, result.ResponseID, result.Items)
		}
	}
	m.mu.Lock()
	if m.entries[entry.bucket] != entry || !entry.busy || entry.generation != generation || entry.conn != conn {
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
	delete(m.entries, entry.bucket)
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
	var closeList []*WSConn
	m.mu.Lock()
	now := m.now()
	for bucket, entry := range m.entries {
		if entry.conn == nil {
			continue
		}
		dead := entry.conn.Err() != nil
		if !dead && entry.busy {
			continue
		}
		if dead || now.Sub(entry.lastUse) > m.maxIdle || now.Sub(entry.dialedAt) > m.maxAge {
			delete(m.entries, bucket)
			entry.generation++
			entry.chain = nil
			m.closing++
			closeList = append(closeList, entry.conn)
		}
	}
	m.mu.Unlock()
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
