package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
)

// httptest.Server.Close closes the global pool, so parallel tests need private transports.
func newTestTransport(t *testing.T) *http.Transport {
	t.Helper()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	return transport
}

func newTestClient(t *testing.T, baseURL string, opts ...ClientOption) *Client {
	t.Helper()
	opts = append([]ClientOption{WithHTTPClient(&http.Client{
		Transport: newTestTransport(t),
		Timeout:   30 * time.Second,
	})}, opts...)
	client, err := NewClient(baseURL, opts...)
	require.NoError(t, err)
	return client
}

func TestNewTestClientUsesPrivateTransport(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, "http://127.0.0.1:1")
	require.NotNil(t, client.httpClient.Transport)
	assert.NotSame(t, http.DefaultTransport, client.httpClient.Transport)
	assert.NotSame(t, client.httpClient.Transport, newTestClient(t, "http://127.0.0.1:1").httpClient.Transport)
	assert.Equal(t, 30*time.Second, client.httpClient.Timeout)

	timed := newTestClient(t, "http://127.0.0.1:1", WithTimeout(100*time.Millisecond))
	assert.Equal(t, 100*time.Millisecond, timed.httpClient.Timeout)
	assert.NotSame(t, http.DefaultTransport, timed.httpClient.Transport)

	streaming := timed.streamingHTTPClient()
	assert.Zero(t, streaming.Timeout)
	assert.Same(t, timed.httpClient.Transport, streaming.Transport)
}

// TestClient_StreamSessionEvents_DeliversMultipleEvents verifies that the
// SSE stream stays open across multiple events instead of being torn down
// when StreamSessionEvents returns. This is a regression test for a bug
// where a deferred cancel() on the streaming context killed the in-flight
// HTTP request as soon as the function returned, turning the stream into
// a one-shot read.
func TestClient_StreamSessionEvents_DeliversMultipleEvents(t *testing.T) {
	t.Parallel()

	// proceed gates each subsequent event on the client having consumed
	// the previous one, guaranteeing the events arrive in separate reads
	// (the one-shot-read regression) without timing dependence. Buffered
	// so a failing run leaves the reader loop unblocked instead of
	// deadlocking the test.
	proceed := make(chan struct{}, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("ResponseWriter must support flushing")
			return
		}

		for i := 1; i <= 3; i++ {
			if i > 1 {
				if _, ok := <-proceed; !ok {
					return
				}
			}
			fmt.Fprintf(w, "data: {\"type\":\"session_title\",\"session_id\":\"s\",\"title\":\"t%d\"}\n\n", i)
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(proceed) })

	c := newTestClient(t, srv.URL)

	ch, err := c.StreamSessionEvents(t.Context(), "s")
	require.NoError(t, err)

	var titles []string
	for ev := range ch {
		titleEv, ok := ev.(*agentruntime.SessionTitleEvent)
		if !ok {
			continue
		}
		titles = append(titles, titleEv.Title)
		if len(titles) < 3 {
			proceed <- struct{}{}
		}
	}

	assert.Equal(t, []string{"t1", "t2", "t3"}, titles)
}

// TestClient_StreamSessionEvents_StopsWhenContextCancelled verifies that
// cancelling the caller's context tears down the stream and closes the
// returned channel.
func TestClient_StreamSessionEvents_StopsWhenContextCancelled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				fmt.Fprint(w, "data: {\"type\":\"session_title\",\"session_id\":\"s\",\"title\":\"x\"}\n\n")
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	ch, err := c.StreamSessionEvents(ctx, "s")
	require.NoError(t, err)

	// Drain at least one event to confirm the stream is live.
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no events received before cancel")
	}

	cancel()

	// Channel must close in a bounded time after cancel.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("channel was not closed after context cancel")
		}
	}
}

func TestClient_RunAgentIgnoresTotalHTTPTimeout(t *testing.T) {
	t.Parallel()

	proceed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"warning\",\"message\":\"first\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-proceed:
			fmt.Fprint(w, "data: {\"type\":\"warning\",\"message\":\"last\"}\n\ndata: {\"type\":\"stream_stopped\"}\n\n")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL, WithHTTPClient(&http.Client{Timeout: 100 * time.Millisecond, Transport: newTestTransport(t)}))
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	stream, err := c.RunAgent(ctx, "s", "agent.yaml", nil, "")
	require.NoError(t, err)
	first := awaitEvent[*agentruntime.WarningEvent](t, stream, "first event")
	assert.Equal(t, "first", first.Message)
	// Let the configured total timeout expire while the stream is healthy.
	<-time.After(200 * time.Millisecond)
	close(proceed)
	var got []agentruntime.Event
	for event := range stream {
		got = append(got, event)
	}
	require.Len(t, got, 2)
	assert.IsType(t, &agentruntime.StreamStoppedEvent{}, got[1])
	last, ok := got[0].(*agentruntime.WarningEvent)
	require.True(t, ok, "got %T", got[0])
	assert.Equal(t, "last", last.Message)
	assert.Equal(t, 100*time.Millisecond, c.httpClient.Timeout, "do not mutate the supplied client")
}

func TestClient_StreamSessionEventsReconnectsFromLastDeliveredID(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		switch calls.Add(1) {
		case 1:
			assert.Empty(t, r.Header.Get("Last-Event-ID"))
			fmt.Fprint(w, "id: 1\ndata: {\"type\":\"session_title\",\"title\":\"one\"}\n\n")
			fmt.Fprint(w, "id: 2\ndata: {\"type\":\"future_event\"}\n\n")
		case 2:
			assert.Equal(t, "2", r.Header.Get("Last-Event-ID"), "unknown events advance the cursor too")
			fmt.Fprint(w, "id: 2\ndata: {\"type\":\"session_title\",\"title\":\"duplicate\"}\n\n")
			fmt.Fprint(w, "id: 3\ndata: {\"type\":\"session_title\",\"title\":\"three\"}\n\n")
			fmt.Fprint(w, "id: 4\ndata: {\"type\":\"session_exited\"}\n\n")
		default:
			t.Error("reconnected after session_exited")
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL, WithAuthToken("secret"))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream, err := c.StreamSessionEvents(ctx, "s")
	require.NoError(t, err)
	var titles []string
	for event := range stream {
		title, ok := event.(*agentruntime.SessionTitleEvent)
		require.True(t, ok, "got %T", event)
		titles = append(titles, title.Title)
	}
	assert.Equal(t, []string{"one", "three"}, titles)
	assert.Equal(t, int32(2), calls.Load())
}

func TestClient_StreamSessionEventsGapRequiresSnapshot(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "7", r.Header.Get("Last-Event-ID"))
		assert.Empty(t, r.URL.Query().Get("since"))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"gap\"}\n\nid: 99\ndata: {\"type\":\"session_title\",\"title\":\"partial history\"}\n\n")
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL+"?since=1")
	stream, err := c.StreamSessionEventsSince(t.Context(), "s", 7)
	require.NoError(t, err)
	var got []agentruntime.Event
	for event := range stream {
		got = append(got, event)
	}
	require.Len(t, got, 1)
	failure, ok := got[0].(*agentruntime.ErrorEvent)
	require.True(t, ok)
	assert.Contains(t, failure.Error, "reload the session snapshot")
	assert.Equal(t, int32(1), calls.Load())
}

func TestClient_SSECancelWithUnreadFullBuffer(t *testing.T) {
	t.Parallel()

	for _, run := range []bool{false, true} {
		t.Run(fmt.Sprintf("run=%v", run), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for i := range 2 * defaultEventChannelCapacity {
					fmt.Fprintf(w, "id: %d\ndata: {\"type\":\"warning\",\"message\":\"x\"}\n\n", i+1)
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			t.Cleanup(srv.Close)
			c := newTestClient(t, srv.URL)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			var stream <-chan agentruntime.Event
			var err error
			if run {
				stream, err = c.RunAgent(ctx, "s", "agent.yaml", nil, "")
			} else {
				stream, err = c.StreamSessionEvents(ctx, "s")
			}
			require.NoError(t, err)
			require.Eventually(t, func() bool { return len(stream) == defaultEventChannelCapacity }, 2*time.Second, time.Millisecond)
			cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				for range stream {
				}
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("cancelled stream stayed open")
			}
		})
	}
}

func TestClient_StreamSessionEventsReconnectsAfterEmptyConnection(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, ": ping\n\n")
			return
		}
		assert.Equal(t, "0", r.Header.Get("Last-Event-ID"))
		fmt.Fprint(w, "id: 1\ndata: {\"type\":\"session_title\",\"title\":\"recovered\"}\n\nid: 2\ndata: {\"type\":\"session_exited\"}\n\n")
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	stream, err := c.StreamSessionEvents(ctx, "s")
	require.NoError(t, err)
	var got []agentruntime.Event
	for event := range stream {
		got = append(got, event)
	}
	require.Len(t, got, 1)
	assert.Equal(t, int32(2), calls.Load())
}

func TestClient_StreamSessionEventsIgnoresTotalHTTPTimeout(t *testing.T) {
	t.Parallel()

	proceed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 1\ndata: {\"type\":\"session_title\",\"title\":\"first\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-proceed:
			fmt.Fprint(w, "id: 2\ndata: {\"type\":\"session_title\",\"title\":\"last\"}\n\nid: 3\ndata: {\"type\":\"session_exited\"}\n\n")
		case <-req.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	client := newTestClient(t, srv.URL, WithTimeout(100*time.Millisecond))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	stream, err := client.StreamSessionEvents(ctx, "s")
	require.NoError(t, err)
	first := awaitEvent[*agentruntime.SessionTitleEvent](t, stream, "first event")
	assert.Equal(t, "first", first.Title)
	<-time.After(200 * time.Millisecond)
	close(proceed)
	var got []agentruntime.Event
	for event := range stream {
		got = append(got, event)
	}
	require.Len(t, got, 1)
	last, ok := got[0].(*agentruntime.SessionTitleEvent)
	require.True(t, ok, "got %T", got[0])
	assert.Equal(t, "last", last.Title)
}

func TestClient_RunAgentIncompleteStreamIsAnError(t *testing.T) {
	t.Parallel()

	for _, data := range []string{
		"",
		`{"type":"agent_choice","content":"partial"}`,
		`{"type":"stream_stopped","session_id":"child"}`,
		`{"type":"stream_stopped",`,
	} {
		t.Run(data, func(t *testing.T) {
			t.Parallel()
			var runs atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				runs.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if data != "" {
					fmt.Fprintf(w, "data: %s\n\n", data)
				}
			}))
			t.Cleanup(srv.Close)
			client := newTestClient(t, srv.URL)
			stream, err := client.RunAgent(t.Context(), "s", "agent.yaml", nil, "")
			require.NoError(t, err)
			var got []agentruntime.Event
			for event := range stream {
				got = append(got, event)
			}
			require.NotEmpty(t, got)
			assert.IsType(t, &agentruntime.ErrorEvent{}, got[len(got)-1])
			assert.Equal(t, int32(1), runs.Load(), "never resubmit a truncated run")
		})
	}
}

func TestClient_RunAgentToolCallsRetracted(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"tool_calls_retracted\",\"tool_call_ids\":[\"declined-tool\"]}\n\ndata: {\"type\":\"stream_stopped\"}\n\n")
	}))
	t.Cleanup(srv.Close)

	client := newTestClient(t, srv.URL)
	stream, err := client.RunAgent(t.Context(), "s", "agent.yaml", nil, "")
	require.NoError(t, err)
	var events []agentruntime.Event
	for event := range stream {
		events = append(events, event)
	}
	require.Len(t, events, 2)
	retracted, ok := events[0].(*agentruntime.ToolCallsRetractedEvent)
	require.True(t, ok, "got %T", events[0])
	assert.Equal(t, []string{"declined-tool"}, retracted.ToolCallIDs)
	assert.IsType(t, &agentruntime.StreamStoppedEvent{}, events[1])
}

func TestAgentChoiceReasoningResetWireIdentity(t *testing.T) {
	t.Parallel()
	want := agentruntime.AgentChoiceReasoningReset("root", "session", "message")
	data, err := json.Marshal(want)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(data, &wire))
	delete(wire, "timestamp")
	require.Equal(t, map[string]any{"type": "agent_choice_reasoning_reset", "agent_name": "root", "session_id": "session", "message_id": "message"}, wire)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: {\"type\":\"stream_stopped\"}\n\n", data)
	}))
	t.Cleanup(srv.Close)
	stream, err := newTestClient(t, srv.URL).RunAgent(t.Context(), "session", "agent.yaml", nil, "")
	require.NoError(t, err)
	var events []agentruntime.Event
	for e := range stream {
		events = append(events, e)
	}
	require.Len(t, events, 2)
	event := events[0]
	reset, ok := event.(*agentruntime.AgentChoiceReasoningResetEvent)
	require.True(t, ok)
	require.Equal(t, "message", reset.MessageID)
	require.Equal(t, "root", reset.AgentName)
	require.Equal(t, "session", event.(agentruntime.SessionScoped).GetSessionID())
}
