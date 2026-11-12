package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/session"
)

// TestAttachedServer_EventStreamSendsHeartbeats verifies that an idle /events
// stream emits SSE comments so clients can distinguish a quiet session from a
// hung transport.
func TestAttachedServer_EventStreamSendsHeartbeats(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(t.Context(), sess.ID, &fakeRuntime{}, sess)
	sm.RegisterEventSource(sess.ID, func(ctx context.Context, _ func(any)) {
		<-ctx.Done() // a session that stays quiet
	})

	srv := NewWithManager(sm, "")
	srv.heartbeatInterval = 10 * time.Millisecond

	ln, err := Listen(ctx, "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ctx, ln) }()
	addr := "http://" + ln.Addr().String()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/events", http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// With no events at all, the only traffic is heartbeats.
	reader := bufio.NewReader(resp.Body)
	for range 2 {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		if strings.TrimSpace(line) == "" {
			continue
		}
		assert.Equal(t, ": ping", strings.TrimSpace(line))
	}
}

// TestAttachedServer_SnapshotReportsAggregateCost verifies the snapshot
// carries the session's total cost so clients need not sum per-message costs.
func TestAttachedServer_SnapshotReportsAggregateCost(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	sess.AddMessage(session.UserMessage("hello"))
	m1 := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "a"})
	m1.Message.Cost = 0.25
	sess.AddMessage(m1)
	m2 := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "b"})
	m2.Message.Cost = 0.50
	sess.AddMessage(m2)
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(t.Context(), sess.ID, &fakeRuntime{}, sess)

	srv := NewWithManager(sm, "")
	ln, err := Listen(ctx, "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ctx, ln) }()
	addr := "http://" + ln.Addr().String()

	resp := httpDoTCP(t, ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/snapshot", nil)

	var snap api.SessionSnapshotResponse
	require.NoError(t, json.Unmarshal(resp, &snap))
	assert.InDelta(t, 0.75, snap.Cost, 1e-9)
}

// TestAttachedServer_SnapshotUnknownSessionCarriesErrorCode verifies that a
// snapshot of an unknown session 404s with a machine-readable code, so
// clients can tell it apart from a route-less 404 (older binary).
func TestAttachedServer_SnapshotUnknownSessionCarriesErrorCode(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	sm := NewSessionManager(ctx, config.Sources{}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{})
	srv := NewWithManager(sm, "")
	ln, err := Listen(ctx, "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ctx, ln) }()
	addr := "http://" + ln.Addr().String()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/api/sessions/nope/snapshot", http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	var body api.ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, api.ErrCodeUnknownSession, body.Code)
}
