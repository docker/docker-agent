package openai

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Replies use the request's model as an ID so crossed streams cannot pass unnoticed.
func newLeaseTestPool(t *testing.T, reply bool) (*wsPool, <-chan string) {
	t.Helper()
	requests := make(chan string, 100)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			var request struct {
				Model string `json:"model"`
			}
			if err := conn.ReadJSON(&request); err != nil {
				return
			}
			requests <- request.Model
			if reply {
				if err := conn.WriteJSON(completedEvent(request.Model)); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	pool := newWSPool("ws"+strings.TrimPrefix(srv.URL, "http"), func(context.Context) (http.Header, error) {
		return http.Header{}, nil
	})
	t.Cleanup(pool.Close)
	return pool, requests
}

func leaseTestStream(t *testing.T, pool *wsPool, id string) *pooledStream {
	t.Helper()
	params := defaultTestParams()
	params.Model = id
	stream, err := pool.Stream(t.Context(), params)
	require.NoError(t, err)
	pooled, ok := stream.(*pooledStream)
	require.True(t, ok)
	t.Cleanup(func() { assert.NoError(t, stream.Close()) })
	return pooled
}

func TestWSPool_ExclusiveStreamLease(t *testing.T) {
	t.Parallel()
	pool, _ := newLeaseTestPool(t, true)

	// Seed the idle slot so this exercises both the reuse and fresh-dial paths.
	seed := leaseTestStream(t, pool, "seed")
	drainStream(t, seed)
	first := leaseTestStream(t, pool, "first")
	second := leaseTestStream(t, pool, "second")
	require.NotSame(t, first.inner.conn, second.inner.conn, "active streams must own different connections")

	var wg sync.WaitGroup
	for _, stream := range []*pooledStream{first, second} {
		wg.Go(func() {
			for stream.Next() {
			}
		})
	}
	wg.Wait()
	assert.NoError(t, first.Err())
	assert.NoError(t, second.Err())
	assert.Equal(t, "first", first.Current().Response.ID)
	assert.Equal(t, "second", second.Current().Response.ID)
	require.NoError(t, first.Close())
	require.NoError(t, second.Close())

	third := leaseTestStream(t, pool, "third")
	assert.Same(t, first.inner.conn, third.inner.conn, "retain one completed connection for reuse")
	drainStream(t, third)
}

func TestWSPool_CloseBeforeCompletionDiscardsLease(t *testing.T) {
	t.Parallel()
	pool, requests := newLeaseTestPool(t, false)
	first := leaseTestStream(t, pool, "first")
	assert.Equal(t, "first", <-requests)
	require.NoError(t, first.Close())
	second := leaseTestStream(t, pool, "second")
	require.NotSame(t, first.inner.conn, second.inner.conn, "an unread response must not reach a later request")
	assert.False(t, first.Next(), "a closed lease cannot read again")
}

func TestWSPool_RepeatedCloseDoesNotReturnActiveLease(t *testing.T) {
	t.Parallel()
	pool, _ := newLeaseTestPool(t, true)
	first := leaseTestStream(t, pool, "first")
	drainStream(t, first)
	second := leaseTestStream(t, pool, "second")
	require.Same(t, first.inner.conn, second.inner.conn)
	require.NoError(t, first.Close())
	third := leaseTestStream(t, pool, "third")
	require.NotSame(t, second.inner.conn, third.inner.conn)
	drainStream(t, second)
	drainStream(t, third)
}

func TestWSPool_CloseClosesActiveLease(t *testing.T) {
	t.Parallel()
	pool, requests := newLeaseTestPool(t, false)
	stream := leaseTestStream(t, pool, "active")
	assert.Equal(t, "active", <-requests)
	// The deadline bounds a regression without sleeps or a stuck test goroutine.
	require.NoError(t, stream.inner.conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	pool.Close()
	assert.False(t, stream.Next())
	require.ErrorIs(t, stream.Err(), net.ErrClosed)
	require.NoError(t, stream.Close())
	pool.mu.Lock()
	defer pool.mu.Unlock()
	assert.Nil(t, pool.conn, "a lease closed by the pool must not be returned")
}

func TestWSPool_ConcurrentCloseAndRead(t *testing.T) {
	t.Parallel()
	pool, _ := newLeaseTestPool(t, true)
	for i := range 30 {
		stream := leaseTestStream(t, pool, strconv.Itoa(i))
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			for stream.Next() {
			}
		})
		wg.Go(func() {
			<-start
			assert.NoError(t, stream.Close())
		})
		close(start)
		wg.Wait()
	}
}

func TestWSPool_CancelledRequestDoesNotConsumeIdleConnection(t *testing.T) {
	t.Parallel()
	pool, _ := newLeaseTestPool(t, true)
	first := leaseTestStream(t, pool, "first")
	drainStream(t, first)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := pool.Stream(ctx, defaultTestParams())
	require.ErrorIs(t, err, context.Canceled)
	second := leaseTestStream(t, pool, "second")
	assert.Same(t, first.inner.conn, second.inner.conn)
	drainStream(t, second)
}

func TestWSPool_ConcurrentRequests(t *testing.T) {
	t.Parallel()
	pool, _ := newLeaseTestPool(t, true)
	const count = 8
	streams := make([]responseEventStream, count)
	errs := make([]error, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			<-start
			params := defaultTestParams()
			params.Model = strconv.Itoa(i)
			streams[i], errs[i] = pool.Stream(t.Context(), params)
		})
	}
	close(start)
	wg.Wait()
	connections := make(map[*websocket.Conn]bool)
	for i, stream := range streams {
		require.NoError(t, errs[i])
		t.Cleanup(func() { assert.NoError(t, stream.Close()) })
		conn := stream.(*pooledStream).inner.conn
		require.False(t, connections[conn], "connection already leased to another request")
		connections[conn] = true
	}
	for _, stream := range streams {
		wg.Go(func() {
			for stream.Next() {
			}
			assert.NoError(t, stream.Close())
		})
	}
	wg.Wait()
	for i, stream := range streams {
		require.NoError(t, stream.Err())
		assert.Equal(t, strconv.Itoa(i), stream.Current().Response.ID)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	assert.Empty(t, pool.active)
	assert.NotNil(t, pool.conn)
}

func TestWSPool_ExpiredAndBrokenIdleConnections(t *testing.T) {
	t.Parallel()
	for _, expired := range []bool{true, false} {
		t.Run(strconv.FormatBool(expired), func(t *testing.T) {
			t.Parallel()
			pool, _ := newLeaseTestPool(t, true)
			first := leaseTestStream(t, pool, "first")
			drainStream(t, first)
			pool.mu.Lock()
			if expired {
				pool.conn.createdAt = time.Now().Add(-wsMaxConnectionAge)
			} else {
				require.NoError(t, pool.conn.conn.Close())
			}
			pool.mu.Unlock()
			second := leaseTestStream(t, pool, "second")
			require.NotSame(t, first.inner.conn, second.inner.conn)
			drainStream(t, second)
		})
	}
}

func TestWSPool_ErrorResponseDiscardsLease(t *testing.T) {
	t.Parallel()
	srv := testWSServer(t, []map[string]any{{"type": "error", "message": "rejected"}})
	defer srv.Close()
	pool := newWSPool("ws"+strings.TrimPrefix(srv.URL, "http"), func(context.Context) (http.Header, error) {
		return http.Header{}, nil
	})
	defer pool.Close()
	stream := leaseTestStream(t, pool, "bad")
	require.True(t, stream.Next())
	require.Error(t, stream.Err())
	require.NoError(t, stream.Close())
	pool.mu.Lock()
	defer pool.mu.Unlock()
	assert.Nil(t, pool.conn)
	assert.Empty(t, pool.active)
}

func TestWSPool_CloseUnblocksReader(t *testing.T) {
	t.Parallel()
	pool, requests := newLeaseTestPool(t, false)
	stream := leaseTestStream(t, pool, "first")
	assert.Equal(t, "first", <-requests)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		assert.False(t, stream.Next())
	}()
	require.NoError(t, stream.Close())
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("closing an unfinished lease did not unblock its reader")
	}
}

func TestWSPool_ClosePreventsCompletedLeaseReturn(t *testing.T) {
	t.Parallel()
	pool, _ := newLeaseTestPool(t, true)
	stream := leaseTestStream(t, pool, "first")
	require.True(t, stream.Next())
	require.True(t, stream.completed.Load())
	pool.Close()
	require.NoError(t, stream.Close())
	pool.mu.Lock()
	assert.Nil(t, pool.conn)
	assert.Empty(t, pool.active)
	pool.mu.Unlock()
	next := leaseTestStream(t, pool, "next")
	require.NotSame(t, stream.inner.conn, next.inner.conn)
	drainStream(t, next)
}
