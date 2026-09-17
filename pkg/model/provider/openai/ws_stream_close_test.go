package openai

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readSignalConn makes an in-flight socket read observable without timing sleeps.
type readSignalConn struct {
	net.Conn

	reading chan struct{}
	once    sync.Once
}

func (c *readSignalConn) Read(p []byte) (int, error) {
	c.once.Do(func() { close(c.reading) })
	return c.Conn.Read(p)
}

func TestWSStreamCloseUnblocksNext(t *testing.T) {
	t.Parallel()
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	reading := make(chan struct{})
	var wrapped *readSignalConn
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		wrapped = &readSignalConn{Conn: conn, reading: make(chan struct{})}
		return wrapped, nil
	}}
	conn, resp, err := dialer.DialContext(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if resp != nil && resp.Body != nil {
		require.NoError(t, resp.Body.Close())
	}
	require.NoError(t, err)
	defer conn.Close()
	// The handshake has finished; the next read belongs to Next.
	wrapped.once = sync.Once{}
	wrapped.reading = reading
	stream := &wsStream{conn: conn}
	result := make(chan bool, 1)
	go func() { result <- stream.Next() }()
	<-reading
	require.NoError(t, stream.Close())
	select {
	case ok := <-result:
		assert.False(t, ok)
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not interrupt Next")
	}
	assert.False(t, stream.Next())
	require.NoError(t, stream.Close(), "Close must be idempotent")
}

func TestWSStreamConcurrentCloseAndNext(t *testing.T) {
	t.Parallel()
	server := testWSServer(t, []map[string]any{completedEvent("response")})
	defer server.Close()
	for range 20 {
		stream, err := dialWebSocket(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil, defaultTestParams())
		require.NoError(t, err)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			for stream.Next() {
			}
		})
		wg.Go(func() { <-start; assert.NoError(t, stream.Close()) })
		close(start)
		wg.Wait()
	}
}
