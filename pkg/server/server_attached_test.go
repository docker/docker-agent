package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/session"
)

func TestAttachedServer_SteerReachesAttachedRuntime(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	fake := &fakeRuntime{}
	sm.AttachRuntime(sess.ID, fake, sess)

	srv := NewWithManager(sm, "")

	ln, err := Listen(ctx, "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ctx, ln) }()

	addr := "http://" + ln.Addr().String()
	resp := httpDoTCP(t, ctx, http.MethodPost, addr+"/api/sessions/"+sess.ID+"/steer",
		api.SteerSessionRequest{Messages: []api.Message{{Content: "hello"}}})
	assert.Contains(t, string(resp), "queued")
}

func TestAttachedServer_EventStreamEmitsRegisteredEvents(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, &fakeRuntime{}, sess)

	events := make(chan any, 4)
	sm.RegisterEventSource(sess.ID, func(ctx context.Context, send func(any)) {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-events:
				send(ev)
			}
		}
	})

	srv := NewWithManager(sm, "")
	ln, err := Listen(ctx, "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ctx, ln) }()

	addr := "http://" + ln.Addr().String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/events", http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	events <- map[string]string{"type": "hello", "msg": "world"}

	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	require.NoError(t, err)
	assert.Contains(t, line, `"type":"hello"`)
}

func httpDoTCP(t *testing.T, ctx context.Context, method, url string, payload any) []byte {
	t.Helper()

	buf, err := json.Marshal(payload)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(buf))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Less(t, resp.StatusCode, 400, string(out))
	return out
}

// TestAttachedServer_DeleteSessionStopsEventStream verifies that calling
// DeleteSession on an attached session cancels in-flight SSE event streams
// and removes the registered event source so a subsequent GET /events 404s.
func TestAttachedServer_DeleteSessionStopsEventStream(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, &fakeRuntime{}, sess)

	sourceCtxDone := make(chan struct{})
	sm.RegisterEventSource(sess.ID, func(ctx context.Context, _ func(any)) {
		<-ctx.Done()
		close(sourceCtxDone)
	})

	srv := NewWithManager(sm, "")
	ln, err := Listen(ctx, "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ctx, ln) }()

	addr := "http://" + ln.Addr().String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/events", http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.NoError(t, sm.DeleteSession(ctx, sess.ID))

	select {
	case <-sourceCtxDone:
	case <-time.After(2 * time.Second):
		t.Fatal("event source ctx was not cancelled when session was deleted")
	}

	_, ok := sm.GetEventSource(sess.ID)
	assert.False(t, ok, "event source must be removed from the registry on delete")

	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/events", http.NoBody)
	require.NoError(t, err)
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp2.StatusCode)
}

// TestAttachedServer_StatusEndpointReturnsCurrentState verifies that
// GET /api/sessions/:id/status returns the current runtime state snapshot.
func TestAttachedServer_StatusEndpointReturnsCurrentState(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	sess.Title = "Test Session"
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	fake := &fakeRuntime{}
	sm.AttachRuntime(sess.ID, fake, sess)

	srv := NewWithManager(sm, "")

	ln, err := Listen(ctx, "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ctx, ln) }()

	addr := "http://" + ln.Addr().String()
	resp := httpDoTCP(t, ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/status", nil)

	var status api.SessionStatusResponse
	require.NoError(t, json.Unmarshal(resp, &status))

	assert.Equal(t, sess.ID, status.ID)
	assert.Equal(t, "Test Session", status.Title)
	assert.False(t, status.Streaming)
}

// TestAttachedServer_FollowUpWhileIdleReturnsQueuedIdle verifies that
// POST /api/sessions/:id/followup returns status "queued_idle" when the
// agent is not currently streaming.
func TestAttachedServer_FollowUpWhileIdleReturnsQueuedIdle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, &fakeRuntime{}, sess)

	srv := NewWithManager(sm, "")

	ln, err := Listen(ctx, "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ctx, ln) }()

	addr := "http://" + ln.Addr().String()
	resp := httpDoTCP(t, ctx, http.MethodPost, addr+"/api/sessions/"+sess.ID+"/followup",
		api.SteerSessionRequest{Messages: []api.Message{{Content: "hello"}}})

	assert.Contains(t, string(resp), "queued_idle")
}

// TestAttachedServer_ReadyEndpointReturnsImmediatelyWithSession verifies
// that GET /api/ready returns immediately when a session is already attached.
func TestAttachedServer_ReadyEndpointReturnsImmediately(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, &fakeRuntime{}, sess)

	srv := NewWithManager(sm, "")

	ln, err := Listen(ctx, "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ctx, ln) }()

	addr := "http://" + ln.Addr().String()
	resp := httpDoTCP(t, ctx, http.MethodGet, addr+"/api/ready", nil)
	assert.Contains(t, string(resp), "ready")
}

// TestAttachedServer_ReadyEndpointTimesOutWithoutSession verifies
// that GET /api/ready returns 503 when no session is registered within the timeout.
func TestAttachedServer_ReadyEndpointTimesOut(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	srv := NewWithManager(sm, "")

	ln, err := Listen(ctx, "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ctx, ln) }()

	addr := "http://" + ln.Addr().String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/api/ready?timeout=100ms", http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// TestAttachedServer_DeleteWithWaitBlocksUntilStreamStops verifies that
// DELETE /api/sessions/:id?wait=true blocks until the stream goroutine exits.
func TestAttachedServer_DeleteWithWaitBlocksUntilStreamStops(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	fake := &fakeRuntime{streamDelay: 200 * time.Millisecond}
	sm.AttachRuntime(sess.ID, fake, sess)

	srv := NewWithManager(sm, "")

	ln, err := Listen(ctx, "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ctx, ln) }()

	addr := "http://" + ln.Addr().String()

	// Start a stream so the streaming lock is held.
	ch, err := sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{{Content: "hello"}})
	require.NoError(t, err)

	// DELETE with wait should block until stream finishes (200ms).
	resp := httpDoTCP(t, ctx, http.MethodDelete, addr+"/api/sessions/"+sess.ID+"?wait=true&timeout=5s", nil)
	assert.Contains(t, string(resp), "deleted")

	// Stream channel should be drained by now.
	for range ch {
	}
}
