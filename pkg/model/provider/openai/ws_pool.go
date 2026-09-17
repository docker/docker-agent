package openai

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

const (
	// wsMaxConnectionAge is the maximum lifetime of a WebSocket connection.
	// OpenAI enforces a 60-minute limit; we reconnect slightly earlier.
	wsMaxConnectionAge = 55 * time.Minute
)

// wsConnection holds a WebSocket connection together with bookkeeping
// metadata for the connection pool.
type wsConnection struct {
	conn      *websocket.Conn
	createdAt time.Time
}

// isExpired returns true when the connection has been open longer than
// wsMaxConnectionAge.
func (c *wsConnection) isExpired() bool {
	return time.Since(c.createdAt) >= wsMaxConnectionAge
}

// wsPool retains one idle connection. Each active stream owns its connection
// exclusively; overlapping requests dial additional connections.
type wsPool struct {
	mu     sync.Mutex
	conn   *wsConnection              // Idle only; removed while a stream owns it.
	active map[*wsConnection]struct{} // Tracked only for shutdown, never lent out.

	// lastResponseID is the ID of the most recent response completed on
	// this pool. It can be passed as previous_response_id in subsequent
	// requests to enable server-side context caching.
	// It lives on the pool (not wsConnection) so it survives reconnections.
	lastResponseID string

	// wsURL is the WebSocket endpoint (e.g. wss://api.openai.com/v1/responses).
	wsURL string

	// headerFn returns the HTTP headers (including Authorization) for
	// the WebSocket handshake. It is called each time a new connection
	// is established so that short-lived tokens are refreshed.
	headerFn func(ctx context.Context) (http.Header, error)
}

// newWSPool creates a pool for the given WebSocket URL.
func newWSPool(wsURL string, headerFn func(ctx context.Context) (http.Header, error)) *wsPool {
	return &wsPool{
		active:   make(map[*wsConnection]struct{}),
		wsURL:    wsURL,
		headerFn: headerFn,
	}
}

// Stream opens (or reuses) a WebSocket connection, sends a response.create
// message, and returns a responseEventStream that yields server events.
func (p *wsPool) Stream(
	ctx context.Context,
	params responses.ResponseNewParams,
) (responseEventStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Inject previous_response_id for server-side context caching when the
	// caller hasn't already set one and we have a response from an earlier
	// exchange on this pool.
	if p.lastResponseID != "" && !params.PreviousResponseID.Valid() {
		params.PreviousResponseID = param.NewOpt(p.lastResponseID)
	}

	conn := p.conn
	p.conn = nil
	if conn != nil && conn.isExpired() {
		slog.DebugContext(ctx, "Closing expired WebSocket connection", "age", time.Since(conn.createdAt))
		_ = conn.conn.Close()
		conn = nil
	}
	if conn == nil {
		return p.dialLocked(ctx, params)
	}

	stream, err := sendOnExisting(conn.conn, params)
	if err != nil {
		slog.WarnContext(ctx, "Existing WebSocket connection failed, reconnecting", "error", err)
		_ = conn.conn.Close()
		return p.dialLocked(ctx, params)
	}
	p.active[conn] = struct{}{}
	return &pooledStream{pool: p, lease: conn, inner: stream}, nil
}

// dialLocked creates a privately owned connection. Caller must hold p.mu.
func (p *wsPool) dialLocked(ctx context.Context, params responses.ResponseNewParams) (*pooledStream, error) {
	headers, err := p.headerFn(ctx)
	if err != nil {
		return nil, fmt.Errorf("websocket pool: headers: %w", err)
	}
	stream, err := dialWebSocket(ctx, p.wsURL, headers, params)
	if err != nil {
		return nil, err
	}
	conn := &wsConnection{conn: stream.conn, createdAt: time.Now()}
	p.active[conn] = struct{}{}
	return &pooledStream{pool: p, lease: conn, inner: stream}, nil
}

// release returns only completed responses to the idle slot.
func (p *wsPool) release(conn *wsConnection, completed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, active := p.active[conn]
	delete(p.active, conn)
	if active && completed && p.conn == nil {
		p.conn = conn
	} else {
		_ = conn.conn.Close()
	}
}

// closeLocked closes the current connection. lastResponseID is preserved
// on the pool so it survives reconnections. Caller must hold p.mu.
func (p *wsPool) closeLocked() {
	if p.conn == nil {
		return
	}
	_ = p.conn.conn.Close()
	p.conn = nil
}

// Close shuts down idle and active connections. Later requests may reconnect.
func (p *wsPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeLocked()
	for conn := range p.active {
		_ = conn.conn.Close()
	}
	clear(p.active)
}

// sendOnExisting sends a response.create on an already-open connection and
// returns a wsStream that reads events from it.
func sendOnExisting(conn *websocket.Conn, params responses.ResponseNewParams) (*wsStream, error) {
	if err := sendResponseCreate(conn, params); err != nil {
		return nil, err
	}

	slog.Debug("WebSocket response.create sent (reused connection)")

	return &wsStream{conn: conn}, nil
}

// pooledStream holds an exclusive lease until Close. Only terminal responses
// can be reused; abandoning a stream closes its connection to unblock reads.
type pooledStream struct {
	pool      *wsPool
	lease     *wsConnection
	inner     *wsStream
	closed    atomic.Bool
	completed atomic.Bool
	closeOnce sync.Once
}

var _ responseEventStream = (*pooledStream)(nil)

func (s *pooledStream) Next() bool {
	if s.closed.Load() {
		return false
	}
	ok := s.inner.Next()
	if !ok {
		return false
	}

	// Track response ID from terminal events for future continuation.
	event := s.inner.Current()
	if isTerminalEvent(event.Type) {
		if event.Response.ID != "" {
			s.pool.mu.Lock()
			s.pool.lastResponseID = event.Response.ID
			s.pool.mu.Unlock()
		}
		// inner.Next has set done, so this reader cannot touch the connection again.
		s.completed.Store(true)
	}

	return true
}

func (s *pooledStream) Current() responses.ResponseStreamEventUnion {
	return s.inner.Current()
}

func (s *pooledStream) Err() error {
	return s.inner.Err()
}

// Close returns a completed lease or discards an unfinished one. Safe during Next.
func (s *pooledStream) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.pool.release(s.lease, s.completed.Load())
	})
	return nil
}
