package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
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
		api.SteerSessionRequest{Messages: []api.Message{{Content: "hello"}}, Framing: "replacement"})
	assert.Contains(t, string(resp), "queued")

	// The request-level framing must survive JSON binding and reach the runtime.
	steered := fake.steeredMessages()
	require.Len(t, steered, 1)
	assert.Equal(t, "hello", steered[0].Content)
	assert.Equal(t, runtime.FramingReplacement, steered[0].Framing)
}

// TestAttachedServer_SteerForwardsDefaultAndPlainFraming verifies that an
// omitted framing field binds to the zero value (which the runtime resolves to
// instruction at drain time) and that an explicit "plain" survives JSON
// binding for programmatic callers.
func TestAttachedServer_SteerForwardsDefaultAndPlainFraming(t *testing.T) {
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

	// Omitted framing must be accepted (no 400) and arrive as the zero value.
	resp := httpDoTCP(t, ctx, http.MethodPost, addr+"/api/sessions/"+sess.ID+"/steer",
		api.SteerSessionRequest{Messages: []api.Message{{Content: "default one"}}})
	assert.Contains(t, string(resp), "queued")

	// Explicit plain framing is forwarded verbatim.
	resp = httpDoTCP(t, ctx, http.MethodPost, addr+"/api/sessions/"+sess.ID+"/steer",
		api.SteerSessionRequest{Messages: []api.Message{{Content: "plain one"}}, Framing: "plain"})
	assert.Contains(t, string(resp), "queued")

	steered := fake.steeredMessages()
	require.Len(t, steered, 2)
	assert.Equal(t, runtime.Framing(""), steered[0].Framing, "omitted framing arrives as the zero value")
	assert.Equal(t, runtime.FramingPlain, steered[1].Framing)
}

// TestAttachedServer_SteerRejectsInvalidFraming verifies the steer endpoint
// rejects an unrecognized framing value with 400 before reaching the runtime.
func TestAttachedServer_SteerRejectsInvalidFraming(t *testing.T) {
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
	buf, err := json.Marshal(api.SteerSessionRequest{
		Messages: []api.Message{{Content: "hello"}},
		Framing:  "bogus",
	})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		addr+"/api/sessions/"+sess.ID+"/steer", bytes.NewReader(buf))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	httpResp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer httpResp.Body.Close()
	body, err := io.ReadAll(httpResp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusBadRequest, httpResp.StatusCode, string(body))
	assert.Contains(t, string(body), "invalid framing")
	assert.Empty(t, fake.steeredMessages(), "an invalid framing must be rejected before reaching the runtime")
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

	// Each event is delivered as an "id: <seq>" line followed by a "data:"
	// line. Scan until we see the data payload.
	reader := bufio.NewReader(resp.Body)
	var sawID bool
	var dataLine string
	for {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		if strings.HasPrefix(line, "id: ") {
			sawID = true
		}
		if strings.HasPrefix(line, "data: ") {
			dataLine = line
			break
		}
	}
	assert.True(t, sawID, "event must carry an SSE id (sequence number)")
	assert.Contains(t, dataLine, `"type":"hello"`)
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

	sourceStarted := make(chan struct{})
	sourceCtxDone := make(chan struct{})
	sm.RegisterEventSource(sess.ID, func(ctx context.Context, _ func(any)) {
		close(sourceStarted)
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

	// Wait for the SSE handler to actually invoke the registered source.
	// Otherwise DeleteSession may remove the source from the registry before
	// StreamEvents picks it up, leaving sourceCtxDone unclosed.
	select {
	case <-sourceStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("event source was never invoked by the SSE handler")
	}

	require.NoError(t, sm.DeleteSession(ctx, sess.ID))

	select {
	case <-sourceCtxDone:
	case <-time.After(10 * time.Second):
		t.Fatal("event source ctx was not cancelled when session was deleted")
	}

	_, ok := sm.runtimeSessions.Load(sess.ID)
	assert.False(t, ok, "runtime must be removed from the registry on delete")
	assert.False(t, sm.HasEventSource(sess.ID), "event source must be removed from the registry on delete")

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
	ch, err := sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{{Content: "hello"}}, "")
	require.NoError(t, err)

	// DELETE with wait should block until stream finishes (200ms).
	resp := httpDoTCP(t, ctx, http.MethodDelete, addr+"/api/sessions/"+sess.ID+"?wait=true&timeout=5s", nil)
	assert.Contains(t, string(resp), "deleted")

	// Stream channel should be drained by now.
	for range ch {
	}
}

// TestAttachedServer_EventStreamReplaysFromLastEventID verifies that an
// /events client which reconnects with a Last-Event-ID (or ?since=) replays
// only the events newer than that sequence number, each tagged with an SSE id.
func TestAttachedServer_EventStreamReplaysFromLastEventID(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, &fakeRuntime{}, sess)

	events := make(chan any, 8)
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

	// Buffer three events before any client connects.
	events <- map[string]string{"type": "one"}
	events <- map[string]string{"type": "two"}
	events <- map[string]string{"type": "three"}
	require.Eventually(t, func() bool {
		seq, ok := sm.LastEventSeq(sess.ID)
		return ok && seq == 3
	}, 2*time.Second, time.Millisecond)

	// Reconnect requesting everything after seq 1: expect only two & three.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/events?since=1", http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	ids, types := readSSE(t, resp.Body, 2)
	assert.Equal(t, []string{"2", "3"}, ids)
	assert.Equal(t, []string{"two", "three"}, types)
}

// TestAttachedServer_EventStreamSignalsGapWhenResumePointEvicted verifies a
// reconnect whose resume point has fallen out of the buffer receives a
// {"type":"gap"} marker (with no id) before the replay.
func TestAttachedServer_EventStreamSignalsGapWhenResumePointEvicted(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, &fakeRuntime{}, sess)

	// Tiny buffer so early events are evicted.
	_, pumpCancel := context.WithCancel(t.Context())
	defer pumpCancel()
	log := newEventLog(2)
	sm.eventLogs.Store(sess.ID, &pumpedEventLog{log: log, cancel: pumpCancel})

	for _, ty := range []string{"a", "b", "c", "d"} { // seqs 1..4; only 3,4 remain
		log.append(map[string]string{"type": ty})
	}

	srv := NewWithManager(sm, "")
	ln, err := Listen(ctx, "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ctx, ln) }()
	addr := "http://" + ln.Addr().String()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/events?since=1", http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// First payload is the gap marker with no id, then events 3 and 4.
	ids, types := readSSE(t, resp.Body, 3)
	assert.Equal(t, []string{"", "3", "4"}, ids)
	assert.Equal(t, []string{"gap", "c", "d"}, types)
}

// readSSE reads n SSE "data:" payloads (each optionally preceded by an "id:"
// line) and returns the ids and the JSON "type" field of each payload.
func readSSE(t *testing.T, r io.Reader, n int) (ids, types []string) {
	t.Helper()
	reader := bufio.NewReader(r)
	pendingID := ""
	for len(types) < n {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		switch {
		case strings.HasPrefix(line, "id: "):
			pendingID = strings.TrimSpace(strings.TrimPrefix(line, "id: "))
		case strings.HasPrefix(line, "data: "):
			var payload struct {
				Type string `json:"type"`
			}
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload))
			ids = append(ids, pendingID)
			types = append(types, payload.Type)
			pendingID = ""
		}
	}
	return ids, types
}

// TestAttachedServer_SnapshotReturnsStateAndLastEventSeq verifies the snapshot
// endpoint returns the session's state together with the latest event
// sequence number, so a client can resync then tail /events?since= gaplessly.
func TestAttachedServer_SnapshotReturnsStateAndLastEventSeq(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	sess.Title = "Snapshot Test"
	sess.AddMessage(session.UserMessage("hello"))
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

	// Two events buffered.
	events <- map[string]string{"type": "one"}
	events <- map[string]string{"type": "two"}
	require.Eventually(t, func() bool {
		seq, ok := sm.LastEventSeq(sess.ID)
		return ok && seq == 2
	}, 2*time.Second, time.Millisecond)

	resp := httpDoTCP(t, ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/snapshot", nil)

	var snap api.SessionSnapshotResponse
	require.NoError(t, json.Unmarshal(resp, &snap))

	assert.Equal(t, sess.ID, snap.ID)
	assert.Equal(t, "Snapshot Test", snap.Title)
	assert.False(t, snap.Streaming)
	assert.Len(t, snap.Messages, 1)
	assert.Equal(t, uint64(2), snap.LastEventSeq)

	// Tailing from the snapshot's seq yields only newer events.
	events <- map[string]string{"type": "three"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		addr+"/api/sessions/"+sess.ID+"/events?since="+strconv.FormatUint(snap.LastEventSeq, 10), http.NoBody)
	require.NoError(t, err)
	streamResp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer streamResp.Body.Close()

	ids, types := readSSE(t, streamResp.Body, 1)
	assert.Equal(t, []string{"3"}, ids)
	assert.Equal(t, []string{"three"}, types)
}

// TestAttachedServer_DeleteEmitsSessionExited verifies that deleting a session
// delivers a terminal session_exited event to a connected /events client
// before the stream closes.
func TestAttachedServer_DeleteEmitsSessionExited(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, &fakeRuntime{}, sess)
	sm.RegisterEventSource(sess.ID, func(ctx context.Context, _ func(any)) {
		<-ctx.Done()
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

	// Give the SSE handler a moment to register, then delete the session.
	require.Eventually(t, func() bool {
		return sm.HasEventSource(sess.ID)
	}, 2*time.Second, time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, sm.DeleteSession(ctx, sess.ID))

	// The client must receive a terminal session_exited event.
	_, types := readSSE(t, resp.Body, 1)
	assert.Equal(t, []string{"session_exited"}, types)
}

// TestAttachedServer_FollowUpIdempotencyKeyDedupes verifies that two HTTP
// follow-ups carrying the same Idempotency-Key deliver the prompt once; the
// retry is acknowledged as a duplicate.
func TestAttachedServer_FollowUpIdempotencyKeyDedupes(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, &fakeRuntime{}, sess)

	var mu sync.Mutex
	var delivered []string
	sm.RegisterFollowUpInjector(sess.ID, func(_ context.Context, content string) {
		mu.Lock()
		delivered = append(delivered, content)
		mu.Unlock()
	})

	srv := NewWithManager(sm, "")
	ln, err := Listen(ctx, "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ctx, ln) }()
	addr := "http://" + ln.Addr().String()

	post := func() api.FollowUpResponse {
		body, mErr := json.Marshal(api.SteerSessionRequest{Messages: []api.Message{{Content: "do it"}}})
		require.NoError(t, mErr)
		req, rErr := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/api/sessions/"+sess.ID+"/followup", bytes.NewReader(body))
		require.NoError(t, rErr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "retry-1")
		resp, dErr := http.DefaultClient.Do(req)
		require.NoError(t, dErr)
		defer resp.Body.Close()
		var out api.FollowUpResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		return out
	}

	first := post()
	assert.False(t, first.Duplicate)

	second := post()
	assert.True(t, second.Duplicate, "retry with same Idempotency-Key is a duplicate")
	assert.Equal(t, "duplicate", second.Status)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"do it"}, delivered, "the follow-up is delivered exactly once")
}

// TestAttachedServer_StatusWaitBlocksUntilAttached verifies that
// GET /status?wait= blocks until the session's runtime is attached, then
// returns its state.
func TestAttachedServer_StatusWaitBlocksUntilAttached(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sess := session.New()
	sess.Title = "Pending"
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	srv := NewWithManager(sm, "")
	ln, err := Listen(ctx, "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ctx, ln) }()
	addr := "http://" + ln.Addr().String()

	// Attach a little later, while the request is already waiting.
	go func() {
		time.Sleep(80 * time.Millisecond)
		sm.AttachRuntime(sess.ID, &fakeRuntime{}, sess)
	}()

	resp := httpDoTCP(t, ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/status?wait=5s", nil)
	var status api.SessionStatusResponse
	require.NoError(t, json.Unmarshal(resp, &status))
	assert.Equal(t, sess.ID, status.ID)
	assert.Equal(t, "Pending", status.Title)
}

// TestAttachedServer_StatusWaitTimesOut verifies a 503 when the session never
// attaches within the wait window.
func TestAttachedServer_StatusWaitTimesOut(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm := NewSessionManager(ctx, config.Sources{}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{})

	srv := NewWithManager(sm, "")
	ln, err := Listen(ctx, "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ctx, ln) }()
	addr := "http://" + ln.Addr().String()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/api/sessions/never/status?wait=100ms", http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}
