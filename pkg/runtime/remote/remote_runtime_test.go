package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// runStreamRecordingClient is a stubBackend variant that records the
// model ref forwarded to RunAgent / RunAgentWithAgentName and lets the test
// inject a synthetic dispatch error so the early-error path is exercised.
type runStreamRecordingClient struct {
	stubBackend

	runErr        error
	gotModel      string
	gotInvocation int
}

func (c *runStreamRecordingClient) RunAgent(_ context.Context, _, _ string, _ []api.Message, model string) (<-chan agentruntime.Event, error) {
	c.gotInvocation++
	c.gotModel = model
	if c.runErr != nil {
		return nil, c.runErr
	}
	ch := make(chan agentruntime.Event)
	close(ch)
	return ch, nil
}

func (c *runStreamRecordingClient) RunAgentWithAgentName(ctx context.Context, sessionID, agent, _ string, msgs []api.Message, model string) (<-chan agentruntime.Event, error) {
	return c.RunAgent(ctx, sessionID, agent, msgs, model)
}

// TestRuntime_SetAgentModel_RetainsOverrideOnDispatchError pins the
// fix for the silent-drop bug: when the next RunStream's HTTP dispatch
// fails (network, auth, server unavailable), the queued override MUST
// remain queued so the next attempt still applies the user's chosen
// model. Clearing it eagerly would silently swallow the request.
func TestRuntime_SetAgentModel_RetainsOverrideOnDispatchError(t *testing.T) {
	t.Parallel()

	client := &runStreamRecordingClient{
		stubBackend: stubBackend{
			cfg: &latest.Config{Agents: latest.Agents{{Name: "test"}}},
		},
		runErr: errors.New("dial tcp: connection refused"),
	}
	rt, err := NewRuntime(client)
	require.NoError(t, err)

	require.NoError(t, rt.SetAgentModel(t.Context(), "test", "openai/gpt-4o"))

	// First RunStream fails to dispatch — drain the error event.
	sess := &session.Session{ID: "s1"}
	for range rt.RunStream(t.Context(), sess) {
	}
	require.Equal(t, 1, client.gotInvocation)
	require.Equal(t, "openai/gpt-4o", client.gotModel)

	// Second RunStream must re-forward the override since the first
	// attempt never reached the server.
	client.runErr = nil
	for range rt.RunStream(t.Context(), sess) {
	}
	require.Equal(t, 2, client.gotInvocation)
	assert.Equal(t, "openai/gpt-4o", client.gotModel, "override must persist after dispatch error")

	// Once successfully forwarded, a subsequent call without a new
	// SetAgentModel must NOT re-send the same override (it is now
	// owned by the server-side session state).
	client.gotModel = "sentinel"
	for range rt.RunStream(t.Context(), sess) {
	}
	require.Equal(t, 3, client.gotInvocation)
	assert.Empty(t, client.gotModel, "override must clear after successful dispatch")
}

// TestRuntime_SetAgentModel_LatestQueuedWins guards the
// concurrent-update path: if SetAgentModel is called between the
// snapshot and the post-dispatch clear, the newer ref must NOT be
// silently overwritten.
func TestRuntime_SetAgentModel_LatestQueuedWins(t *testing.T) {
	t.Parallel()

	rt, err := NewRuntime(&stubBackend{
		cfg: &latest.Config{Agents: latest.Agents{{Name: "test"}}},
	})
	require.NoError(t, err)

	require.NoError(t, rt.SetAgentModel(t.Context(), "test", "first"))
	require.NoError(t, rt.SetAgentModel(t.Context(), "test", "second"))

	rt.pendingMu.Lock()
	got := rt.pendingModelOverride
	rt.pendingMu.Unlock()
	assert.Equal(t, "second", got)
}

// mcpPromptsClient is a stubBackend variant returning MCP prompts the
// way the HTTP client does: decoded into map[string]any, with each prompt a
// nested map — never a concrete tools.PromptInfo.
type mcpPromptsClient struct {
	stubBackend
}

func (c *mcpPromptsClient) GetSessionMCPPrompts(context.Context, string) (map[string]any, error) {
	return map[string]any{
		"review": map[string]any{
			"name":        "review",
			"description": "Review code",
			"arguments": []any{
				map[string]any{"name": "path", "description": "File to review", "required": true},
			},
		},
	}, nil
}

// TestRuntime_CurrentMCPPrompts pins the JSON-decoded-map conversion:
// values arrive as map[string]any, so a plain type assertion would silently
// drop every prompt.
func TestRuntime_CurrentMCPPrompts(t *testing.T) {
	t.Parallel()

	client := &mcpPromptsClient{
		stubBackend: stubBackend{
			cfg: &latest.Config{Agents: latest.Agents{{Name: "test"}}},
		},
	}
	rt, err := NewRuntime(client)
	require.NoError(t, err)
	rt.sessionID = "session-1"

	prompts := rt.CurrentMCPPrompts(t.Context())
	require.Len(t, prompts, 1)
	assert.Equal(t, "review", prompts["review"].Name)
	assert.Equal(t, "Review code", prompts["review"].Description)
	require.Len(t, prompts["review"].Arguments, 1)
	assert.Equal(t, "path", prompts["review"].Arguments[0].Name)
	assert.True(t, prompts["review"].Arguments[0].Required)
}

func TestRuntime_BackgroundEventsSurviveTurnsWithoutReplayingHistory(t *testing.T) {
	t.Parallel()

	var runs, snapshots, subscriptions atomic.Int32
	backgroundReady := make(chan struct{})
	deliverRecall := make(chan struct{})
	backgroundStopped := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/snapshot"):
			snapshots.Add(1)
			fmt.Fprint(w, `{"id":"s","last_event_seq":12}`)
		case strings.HasSuffix(req.URL.Path, "/events"):
			subscriptions.Add(1)
			assert.Equal(t, "12", req.Header.Get("Last-Event-ID"))
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			close(backgroundReady)
			select {
			case <-deliverRecall:
				fmt.Fprint(w, "id: 13\ndata: {\"type\":\"agent_choice\",\"message_id\":\"recall\",\"content\":\"recall answer\"}\n\n")
				w.(http.Flusher).Flush()
			case <-req.Context().Done():
			}
			<-req.Context().Done()
			close(backgroundStopped)
		case req.Method == http.MethodPost:
			runs.Add(1)
			assert.Equal(t, int32(1), snapshots.Load(), "cursor must be taken before RunAgent")
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"agent_choice\",\"message_id\":\"foreground\",\"content\":\"foreground answer\"}\n\ndata: {\"type\":\"stream_stopped\"}\n\n")
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	client := newTestClient(t, srv.URL)
	rt, err := NewRuntime(client)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	background := make(chan agentruntime.Event, 4)
	rt.OnBackgroundEvent(func(event agentruntime.Event) { background <- event })

	ctx, cancel := context.WithCancel(t.Context())
	var foreground []agentruntime.Event
	for event := range rt.RunStream(ctx, &session.Session{ID: "s"}) {
		foreground = append(foreground, event)
	}
	require.Len(t, foreground, 2)
	cancel() // The turn context must not end the session subscription.
	select {
	case <-backgroundReady:
	case <-time.After(2 * time.Second):
		t.Fatal("background subscription did not connect")
	}
	close(deliverRecall)
	select {
	case event := <-background:
		answer, ok := event.(*agentruntime.AgentChoiceEvent)
		require.True(t, ok, "got %T", event)
		assert.Equal(t, "recall answer", answer.Content)
	case <-time.After(2 * time.Second):
		t.Fatal("idle recall was not delivered")
	}

	for range rt.RunStream(t.Context(), &session.Session{ID: "s"}) {
	}
	assert.Equal(t, int32(2), runs.Load(), "subscription must not spawn extra runs")
	assert.Equal(t, int32(1), snapshots.Load(), "do not re-snapshot and skip pending events between turns")
	assert.Equal(t, int32(1), subscriptions.Load())
	assert.Empty(t, background, "foreground answers must not also reach the background sink")
	require.NoError(t, rt.Close())
	select {
	case <-backgroundStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not stop background subscription")
	}
}

func TestRuntime_BackgroundSubscriptionWaitsForEventLogAndDeliversElicitationOnce(t *testing.T) {
	t.Parallel()

	var logAvailable atomic.Bool
	var attempts, runs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/snapshot"):
			fmt.Fprint(w, `{"id":"s","last_event_seq":0}`)
		case strings.HasSuffix(req.URL.Path, "/events"):
			// POST can enable the log as soon as the attempt is published.
			available := logAvailable.Load()
			attempts.Add(1)
			assert.Equal(t, "0", req.Header.Get("Last-Event-ID"))
			if !available {
				http.NotFound(w, req)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "id: 1\ndata: {\"type\":\"elicitation_request\",\"elicitation_id\":\"eid\"}\n\nid: 2\ndata: {\"type\":\"session_exited\"}\n\n")
		case req.Method == http.MethodPost:
			runs.Add(1)
			assert.Eventually(t, func() bool { return attempts.Load() > 0 }, 2*time.Second, time.Millisecond)
			logAvailable.Store(true)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"elicitation_request\",\"elicitation_id\":\"eid\"}\n\ndata: {\"type\":\"stream_stopped\"}\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	client := newTestClient(t, srv.URL)
	rt, err := NewRuntime(client)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	background := make(chan agentruntime.Event, 4)
	rt.OnBackgroundEvent(func(event agentruntime.Event) { background <- event })
	var foreground []agentruntime.Event
	for event := range rt.RunStream(t.Context(), &session.Session{ID: "s"}) {
		foreground = append(foreground, event)
	}
	var requests []*agentruntime.ElicitationRequestEvent
	for _, event := range foreground {
		if request, ok := event.(*agentruntime.ElicitationRequestEvent); ok {
			requests = append(requests, request)
		}
	}
	require.Eventually(t, func() bool {
		rt.backgroundMu.Lock()
		defer rt.backgroundMu.Unlock()
		return rt.background == nil
	}, 3*time.Second, time.Millisecond)
	select {
	case event := <-background:
		request, ok := event.(*agentruntime.ElicitationRequestEvent)
		require.True(t, ok, "got %T", event)
		requests = append(requests, request)
	default:
	}
	require.Len(t, requests, 1, "foreground/background copies must be delivered once")
	assert.Equal(t, "eid", requests[0].ElicitationID)
	assert.Empty(t, background)
	assert.Equal(t, int32(1), runs.Load())
	require.GreaterOrEqual(t, attempts.Load(), int32(2))
}

func TestRuntime_BackgroundGapReconcilesSavedTextWithoutReplay(t *testing.T) {
	t.Parallel()

	var runs, snapshots, subscriptions atomic.Int32
	foregroundDone := make(chan struct{})
	streamReady := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/snapshot"):
			w.Header().Set("Content-Type", "application/json")
			if snapshots.Add(1) == 1 {
				fmt.Fprint(w, `{"id":"s","last_event_seq":10,"messages":[{"agent_name":"root","message":{"role":"assistant","message_id":"old","content":"old answer"}}]}`)
			} else {
				fmt.Fprint(w, `{"id":"s","last_event_seq":30,"messages":[
					{"agent_name":"root","message":{"role":"assistant","message_id":"old","content":"old answer"}},
					{"agent_name":"root","message":{"role":"assistant","message_id":"foreground","content":"foreground answer"}},
					{"agent_name":"root","message":{"role":"assistant","message_id":"recall","content":"recall final","reasoning_content":"PRIVATE reasoning"}},
					{"message":{"role":"tool","message_id":"tool","content":"PRIVATE tool output"}},
					{"message":{"role":"assistant","message_id":"tool-call","content":"PRIVATE tool content","tool_calls":[{"id":"call","type":"function"}]}},
					{"implicit":true,"message":{"role":"assistant","message_id":"implicit","content":"PRIVATE implicit"}}
				]}`)
			}
		case strings.HasSuffix(req.URL.Path, "/events"):
			w.Header().Set("Content-Type", "text/event-stream")
			if subscriptions.Add(1) == 1 {
				assert.Equal(t, "10", req.Header.Get("Last-Event-ID"))
				w.(http.Flusher).Flush()
				select {
				case <-foregroundDone:
				case <-req.Context().Done():
					return
				}
				fmt.Fprint(w, "id: 11\ndata: {\"type\":\"agent_choice\",\"message_id\":\"recall\",\"content\":\"recall \"}\n\ndata: {\"type\":\"gap\"}\n\nid: 20\ndata: {\"type\":\"agent_choice\",\"message_id\":\"recall\",\"content\":\"blind replay\"}\n\n")
				return
			}
			assert.Equal(t, "30", req.Header.Get("Last-Event-ID"))
			// Snapshot/log overlap is possible: the completed ID must not append twice.
			fmt.Fprint(w, "id: 31\ndata: {\"type\":\"agent_choice\",\"message_id\":\"recall\",\"content\":\"recall final\"}\n\nid: 32\ndata: {\"type\":\"agent_choice\",\"message_id\":\"later\",\"content\":\"later answer\"}\n\n")
			w.(http.Flusher).Flush()
			close(streamReady)
			<-req.Context().Done()
		case req.Method == http.MethodPost:
			runs.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"agent_choice\",\"message_id\":\"foreground\",\"content\":\"foreground answer\"}\n\ndata: {\"type\":\"stream_stopped\",\"session_id\":\"s\"}\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	client := newTestClient(t, srv.URL)
	rt, err := NewRuntime(client)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	background := make(chan agentruntime.Event, 16)
	rt.OnBackgroundEvent(func(event agentruntime.Event) { background <- event })
	var foreground []agentruntime.Event
	for event := range rt.RunStream(t.Context(), &session.Session{ID: "s"}) {
		foreground = append(foreground, event)
	}
	require.Len(t, foreground, 2)
	close(foregroundDone)
	select {
	case <-streamReady:
	case <-time.After(3 * time.Second):
		t.Fatal("gap recovery did not reconnect")
	}
	var choices []*agentruntime.AgentChoiceEvent
	var recovered int
	deadline := time.After(3 * time.Second)
	for len(choices) < 3 {
		select {
		case event := <-background:
			switch event := event.(type) {
			case *agentruntime.AgentChoiceEvent:
				choices = append(choices, event)
			case *agentruntime.WarningEvent:
				assert.Contains(t, event.Message, "gap")
			case *agentruntime.SessionRecoveredEvent:
				recovered++
				assert.Equal(t, "s", event.SessionID)
			default:
				t.Fatalf("unexpected event %T", event)
			}
		case <-deadline:
			t.Fatal("saved final answer was not reconciled")
		}
	}
	assert.Equal(t, 1, recovered, "idle recovery must reset lifecycle state exactly once")
	assert.Equal(t, "recall ", choices[0].Content)
	assert.Equal(t, "final", choices[1].Content, "append only the missing suffix")
	assert.Equal(t, "recall", choices[1].MessageID)
	assert.Equal(t, "s", choices[1].SessionID)
	assert.Equal(t, "later answer", choices[2].Content)
	assert.Empty(t, background)
	assert.Equal(t, int32(1), runs.Load())
	assert.Equal(t, int32(2), snapshots.Load())
	assert.Equal(t, int32(2), subscriptions.Load())
}

func TestRemoteMessageHistoryRejectsUnsafeRecovery(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		delivered *agentruntime.AgentChoiceEvent
		savedID   string
		saved     string
	}{
		{name: "unidentified delivered message", delivered: agentruntime.AgentChoice("root", "s", "partial").(*agentruntime.AgentChoiceEvent), savedID: "id", saved: "partial final"},
		{name: "unidentified saved message", delivered: agentruntime.AgentChoice("root", "s", "partial", "id").(*agentruntime.AgentChoiceEvent), saved: "partial final"},
		{name: "changed prefix", delivered: agentruntime.AgentChoice("root", "s", "partial", "id").(*agentruntime.AgentChoiceEvent), savedID: "id", saved: "rewritten final"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			history := newRemoteMessageHistory(nil)
			require.True(t, history.deliver(tc.delivered, func(agentruntime.Event) bool { return true }))
			snapshot := &api.SessionSnapshotResponse{ID: "s", Messages: []session.Message{
				{Message: chat.Message{Role: chat.MessageRoleAssistant, MessageID: "new", Content: "must not partially replay"}},
				{Message: chat.Message{Role: chat.MessageRoleAssistant, MessageID: tc.savedID, Content: tc.saved}},
			}}
			var got []agentruntime.Event
			err := history.reconcile(snapshot, func(event agentruntime.Event) { got = append(got, event) })
			require.Error(t, err)
			assert.Empty(t, got)
		})
	}
}

func TestRuntime_ForegroundEOFHasErrorStop(t *testing.T) {
	t.Parallel()

	client := &runStreamRecordingClient{}
	rt, err := NewRuntime(client)
	require.NoError(t, err)
	var got []agentruntime.Event
	for event := range rt.RunStream(t.Context(), &session.Session{ID: "s"}) {
		got = append(got, event)
	}
	require.Len(t, got, 2)
	assert.IsType(t, &agentruntime.ErrorEvent{}, got[0])
	stop, ok := got[1].(*agentruntime.StreamStoppedEvent)
	require.True(t, ok)
	assert.Equal(t, "error", stop.Reason)
	assert.Equal(t, "s", stop.SessionID)
	assert.Equal(t, 1, client.gotInvocation)
}

func TestRuntime_BackgroundGapWaitsForIdleAndCloseCancelsRecovery(t *testing.T) {
	t.Parallel()

	var runs, snapshots, subscriptions atomic.Int32
	recovering := make(chan struct{})
	recoveryStopped := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/snapshot"):
			switch snapshots.Add(1) {
			case 1:
				fmt.Fprint(w, `{"id":"s","last_event_seq":0}`)
			case 2:
				fmt.Fprint(w, `{"id":"s","last_event_seq":20,"streaming":true,"messages":[{"message":{"role":"assistant","message_id":"id","content":"not final"}}]}`)
			default:
				close(recovering)
				<-req.Context().Done()
				close(recoveryStopped)
			}
		case strings.HasSuffix(req.URL.Path, "/events"):
			subscriptions.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"gap\"}\n\n")
		case req.Method == http.MethodPost:
			runs.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"stream_stopped\"}\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	client := newTestClient(t, srv.URL)
	rt, err := NewRuntime(client)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	background := make(chan agentruntime.Event, 4)
	rt.OnBackgroundEvent(func(event agentruntime.Event) { background <- event })
	for range rt.RunStream(t.Context(), &session.Session{ID: "s"}) {
	}
	select {
	case <-recovering:
	case <-time.After(3 * time.Second):
		t.Fatal("did not wait for an idle snapshot")
	}
	require.NoError(t, rt.Close())
	select {
	case <-recoveryStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel snapshot recovery")
	}
	assert.Equal(t, int32(1), runs.Load())
	assert.Equal(t, int32(1), subscriptions.Load(), "never advance the cursor from a running snapshot")
	for _, event := range drainEvents(background) {
		assert.IsType(t, &agentruntime.WarningEvent{}, event, "partial saved text must not be emitted")
	}
}

func TestRemoteMessageHistoryPreservesKnownSubSessionScope(t *testing.T) {
	t.Parallel()

	history := newRemoteMessageHistory(nil)
	require.True(t, history.deliver(agentruntime.AgentChoice("worker", "child", "partial ", "id"), func(agentruntime.Event) bool { return true }))
	snapshot := &api.SessionSnapshotResponse{ID: "parent", Messages: []session.Message{
		{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, MessageID: "id", Content: "partial final"}},
	}}
	var got []agentruntime.Event
	require.NoError(t, history.reconcile(snapshot, func(event agentruntime.Event) { got = append(got, event) }))
	require.Len(t, got, 1)
	choice, ok := got[0].(*agentruntime.AgentChoiceEvent)
	require.True(t, ok)
	assert.Equal(t, "child", choice.SessionID)
	assert.Equal(t, "final", choice.Content)
}

func TestRemoteMessageHistoryDeduplicatesElicitationEitherDeliveryOrder(t *testing.T) {
	t.Parallel()

	history := newRemoteMessageHistory(nil)
	for _, id := range []string{"foreground-first", "background-first"} {
		request := &agentruntime.ElicitationRequestEvent{ElicitationID: id}
		var deliveries int
		for range 2 {
			require.True(t, history.deliver(request, func(agentruntime.Event) bool {
				deliveries++
				return true
			}))
		}
		assert.Equal(t, 1, deliveries)
	}
}

func TestRemoteMessageHistoryRejectsDuplicateSnapshotIDs(t *testing.T) {
	t.Parallel()

	history := newRemoteMessageHistory(nil)
	snapshot := &api.SessionSnapshotResponse{ID: "s", Messages: []session.Message{
		{Message: chat.Message{Role: chat.MessageRoleAssistant, MessageID: "id", Content: "first"}},
		{Message: chat.Message{Role: chat.MessageRoleAssistant, MessageID: "id", Content: "second"}},
	}}
	var got []agentruntime.Event
	require.Error(t, history.reconcile(snapshot, func(event agentruntime.Event) { got = append(got, event) }))
	assert.Empty(t, got)
}

func TestRuntimeRetiredBackgroundSubscriptionDoesNotEmit(t *testing.T) {
	t.Parallel()
	rt, err := NewRuntime(&stubBackend{})
	require.NoError(t, err)
	canceled := make(chan struct{})
	sub := &remoteEventSubscription{cancel: func() { close(canceled) }, sessionID: "old", history: newRemoteMessageHistory(nil)}
	rt.background = sub
	var delivered []agentruntime.Event
	rt.OnBackgroundEvent(func(event agentruntime.Event) { delivered = append(delivered, event) })
	rt.RetireBackgroundEvents()
	<-canceled
	rt.emitBackgroundEvent(sub, agentruntime.AgentChoice("root", "old", "OLD-ANSWER", "old"))
	require.Empty(t, delivered)
	require.Nil(t, rt.background)
}

func TestRemoteMessageHistoryUsesVisibleContent(t *testing.T) {
	t.Parallel()
	msg := func(id, content string) session.Message {
		return session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, MessageID: id, Content: content}}
	}
	history := newRemoteMessageHistory([]session.Message{msg("old", "visible<tool_call>PRIVATE"), msg("hidden", "<tool_call>PRIVATE")})
	assert.True(t, history.complete["old"])
	assert.False(t, history.complete["hidden"], "hidden-only text must not mask later visible chunks")
	assert.Empty(t, history.content, "baseline needs IDs, not copies of old answers")
	require.True(t, history.deliver(agentruntime.AgentChoice("root", "s", "answer<tool_call>PRIVATE", "new"), func(agentruntime.Event) bool { return true }))
	var got []agentruntime.Event
	require.NoError(t, history.reconcile(&api.SessionSnapshotResponse{ID: "s", Messages: []session.Message{
		msg("old", "visible<tool_call>PRIVATE"), msg("new", "answer<tool_call>PRIVATE MORE"), msg("recovered", "safe<tool_call>SECRET"),
	}}, func(event agentruntime.Event) { got = append(got, event) }))
	require.Len(t, got, 1)
	assert.Equal(t, "safe", got[0].(*agentruntime.AgentChoiceEvent).Content)
	assert.Empty(t, history.content, "finalized answers retain only dedup IDs")
}

func TestRemoteMessageHistoryRetentionFailsClosed(t *testing.T) {
	t.Parallel()
	history := newRemoteMessageHistory(nil)
	for i := range remoteHistoryMaxMessages + 2 {
		require.True(t, history.deliver(agentruntime.AgentChoice("root", "s", "answer", strconv.Itoa(i)), func(agentruntime.Event) bool { return true }))
	}
	assert.Empty(t, history.content)
	assert.Empty(t, history.sessions)
	assert.Zero(t, history.bytes)
	var got []agentruntime.Event
	err := history.reconcile(&api.SessionSnapshotResponse{ID: "s"}, func(event agentruntime.Event) { got = append(got, event) })
	require.ErrorContains(t, err, "retention limit")
	assert.Empty(t, got, "forgotten IDs must never cause blind replay")
}

type recoveryBackend struct {
	runStreamRecordingClient

	snapshot *api.SessionSnapshotResponse
	openErr  error
	streams  chan agentruntime.Event
	opened   chan struct{}
}

func (c *recoveryBackend) GetSessionSnapshot(context.Context, string) (*api.SessionSnapshotResponse, error) {
	return c.snapshot, nil
}

func (c *recoveryBackend) StreamSessionEventsSince(context.Context, string, uint64) (<-chan agentruntime.Event, error) {
	if c.opened != nil {
		close(c.opened)
	}
	return c.streams, c.openErr
}

func TestRuntimeRecoveryStopsReplayAfterRetirement(t *testing.T) {
	t.Parallel()
	client := &recoveryBackend{snapshot: &api.SessionSnapshotResponse{ID: "s", Messages: []session.Message{
		{Message: chat.Message{Role: chat.MessageRoleAssistant, MessageID: "first", Content: "first"}},
		{Message: chat.Message{Role: chat.MessageRoleAssistant, MessageID: "second", Content: "stale"}},
	}}}
	rt, err := NewRuntime(client)
	require.NoError(t, err)
	sub := &remoteEventSubscription{sessionID: "s", history: newRemoteMessageHistory(nil), cancel: func() {}}
	rt.background = sub
	var got []agentruntime.Event
	rt.OnBackgroundEvent(func(event agentruntime.Event) {
		got = append(got, event)
		rt.RetireBackgroundEvents()
	})
	_, err = rt.reconcileIdleSnapshot(t.Context(), sub, client)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, got, 1, "captured handler must not receive the rest of a retired snapshot")
	assert.Equal(t, "first", got[0].(*agentruntime.AgentChoiceEvent).Content)
}

func TestRuntimeFailedBackgroundSubscriptionCanRestart(t *testing.T) {
	t.Parallel()
	client := &recoveryBackend{snapshot: &api.SessionSnapshotResponse{ID: "s"}, openErr: &sessionEventHTTPError{status: http.StatusForbidden}}
	rt, err := NewRuntime(client)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	delivered := make(chan agentruntime.Event, 4)
	rt.OnBackgroundEvent(func(event agentruntime.Event) { delivered <- event })
	rt.startBackgroundEvents(t.Context(), "s")
	select {
	case event := <-delivered:
		assert.IsType(t, &agentruntime.ErrorEvent{}, event)
	case <-time.After(2 * time.Second):
		t.Fatal("subscription did not fail")
	}
	require.Eventually(t, func() bool {
		rt.backgroundMu.Lock()
		defer rt.backgroundMu.Unlock()
		return rt.background == nil
	}, time.Second, time.Millisecond)
	client.openErr = nil
	client.streams = make(chan agentruntime.Event, 1)
	client.streams <- agentruntime.Warning("retry succeeded", "root")
	close(client.streams)
	rt.startBackgroundEvents(t.Context(), "s")
	select {
	case event := <-delivered:
		assert.Equal(t, "retry succeeded", event.(*agentruntime.WarningEvent).Message)
	case <-time.After(2 * time.Second):
		t.Fatal("subscription did not restart")
	}
}

func TestRuntimeRecoveryIsAnIdleResetNotAStop(t *testing.T) {
	t.Parallel()
	client := &recoveryBackend{snapshot: &api.SessionSnapshotResponse{ID: "s", LastEventSeq: 42}}
	rt, err := NewRuntime(client)
	require.NoError(t, err)
	sub := &remoteEventSubscription{sessionID: "s", history: newRemoteMessageHistory(nil), cancel: func() {}}
	rt.background = sub
	depth := map[string]int{}
	var stops, resets int
	rt.OnBackgroundEvent(func(event agentruntime.Event) {
		switch event := event.(type) {
		case *agentruntime.StreamStartedEvent:
			depth[event.SessionID]++
		case *agentruntime.StreamStoppedEvent:
			stops++
			depth[event.SessionID]--
		case *agentruntime.SessionRecoveredEvent:
			resets++
			clear(depth)
		}
	})
	for _, id := range []string{"s", "s", "child"} {
		rt.emitBackgroundEvent(sub, agentruntime.StreamStarted(id, "root"))
	}
	assert.Equal(t, 2, depth["s"])
	_, err = rt.reconcileIdleSnapshot(t.Context(), sub, client)
	require.NoError(t, err)
	assert.Empty(t, depth, "one boundary resets all nested starts lost to the gap")
	assert.Equal(t, 1, resets)
	assert.Zero(t, stops, "recovery cannot trigger queued stop actions")
}

type elicitationRecordingClient struct {
	stubBackend

	mu      sync.Mutex
	ids     []string
	actions []tools.ElicitationAction
}

func (c *elicitationRecordingClient) ResumeElicitation(_ context.Context, _ string, action tools.ElicitationAction, _ map[string]any, id ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, firstElicitationID(id))
	c.actions = append(c.actions, action)
	return nil
}

func TestRuntimeOAuthResponsesAreCorrelated(t *testing.T) {
	t.Parallel()
	client := &elicitationRecordingClient{}
	rt, err := NewRuntime(client)
	require.NoError(t, err)
	rt.sessionID = "s"
	for _, id := range []string{"oauth-a", "oauth-b"} {
		rt.trackOAuthElicitation(&agentruntime.ElicitationRequestEvent{ElicitationID: id, Meta: map[string]any{"docker-agent/type": "oauth_flow"}})
	}
	// A form response must not start the unrelated OAuth flow.
	require.NoError(t, rt.ResumeElicitation(t.Context(), tools.ElicitationActionAccept, nil, "form"))
	assert.Equal(t, []string{"form"}, client.ids)
	require.ErrorContains(t, rt.ResumeElicitation(t.Context(), tools.ElicitationActionAccept, nil), "explicit")
	assert.Len(t, rt.pendingOAuthElicitations, 2)
	// Missing metadata fails before network/browser side effects and declines only A.
	require.ErrorContains(t, rt.ResumeElicitation(t.Context(), tools.ElicitationActionAccept, nil, "oauth-a"), "server_url")
	assert.Equal(t, []string{"form", "oauth-a"}, client.ids)
	assert.Equal(t, tools.ElicitationActionDecline, client.actions[1])
	assert.NotContains(t, rt.pendingOAuthElicitations, "oauth-a")
	assert.Contains(t, rt.pendingOAuthElicitations, "oauth-b")
	require.NoError(t, rt.ResumeElicitation(t.Context(), tools.ElicitationActionDecline, nil, "oauth-b"))
	assert.Empty(t, rt.pendingOAuthElicitations, "declining must not start OAuth")
}

func TestRuntimeDuplicateElicitationDoesNotRestorePendingOAuth(t *testing.T) {
	t.Parallel()
	rt, err := NewRuntime(&elicitationRecordingClient{})
	require.NoError(t, err)
	rt.sessionID = "s"
	sub := &remoteEventSubscription{sessionID: "s", history: newRemoteMessageHistory(nil), cancel: func() {}}
	rt.background = sub
	var got []agentruntime.Event
	rt.OnBackgroundEvent(func(event agentruntime.Event) { got = append(got, event) })
	request := &agentruntime.ElicitationRequestEvent{ElicitationID: "oauth", Meta: map[string]any{"docker-agent/type": "oauth_flow"}}
	rt.emitBackgroundEvent(sub, request)
	require.NoError(t, rt.ResumeElicitation(t.Context(), tools.ElicitationActionDecline, nil, "oauth"))
	rt.emitBackgroundEvent(sub, request)
	assert.Len(t, got, 1)
	assert.Empty(t, rt.pendingOAuthElicitations, "suppressed duplicates must not mutate OAuth state")
}

func TestRemoteMessageHistoryReleasesStoppedTextAndKeepsNestedScope(t *testing.T) {
	t.Parallel()
	history := newRemoteMessageHistory(nil)
	for _, event := range []agentruntime.Event{
		agentruntime.AgentChoice("root", "s", "answer", "root"),
		agentruntime.AgentChoice("root", "s", "<tool_call>PRIVATE", "hidden"),
		agentruntime.AgentChoice("worker", "child", "partial", "child"),
		agentruntime.StreamStopped("s", "root", ""),
	} {
		require.True(t, history.deliver(event, func(agentruntime.Event) bool { return true }))
	}
	assert.True(t, history.complete["root"])
	assert.False(t, history.complete["hidden"])
	assert.NotContains(t, history.content, "root")
	assert.NotContains(t, history.content, "hidden")
	assert.Contains(t, history.content, "child", "parent stop cannot finalize a nested stream")
	assert.Equal(t, len("partial"), history.bytes)
}

func TestRemoteMessageHistoryNestedStopDoesNotCompleteOuterMessage(t *testing.T) {
	t.Parallel()
	history := newRemoteMessageHistory(nil)
	for _, event := range []agentruntime.Event{agentruntime.StreamStarted("s", "root"), agentruntime.StreamStarted("s", "worker"), agentruntime.AgentChoice("root", "s", "partial", "id"), agentruntime.StreamStopped("s", "worker", "")} {
		require.True(t, history.deliver(event, func(agentruntime.Event) bool { return true }))
	}
	assert.False(t, history.complete["id"])
	var got []agentruntime.Event
	require.True(t, history.deliver(agentruntime.AgentChoice("root", "s", " final", "id"), func(event agentruntime.Event) bool { got = append(got, event); return true }))
	require.Len(t, got, 1)
	require.True(t, history.deliver(agentruntime.StreamStopped("s", "root", ""), func(agentruntime.Event) bool { return true }))
	assert.True(t, history.complete["id"])
	assert.Empty(t, history.content)
	assert.Empty(t, history.streamDepth)
}

func TestRemoteMessageHistoryElicitationRetentionDoesNotPermitDuplicates(t *testing.T) {
	t.Parallel()
	history := newRemoteMessageHistory(nil)
	for i := range remoteHistoryMaxElicitations {
		history.elicitations[strconv.Itoa(i)] = true
	}
	var got []agentruntime.Event
	request := &agentruntime.ElicitationRequestEvent{ElicitationID: "overflow", Meta: map[string]any{"docker-agent/type": "oauth_flow"}}
	require.False(t, history.deliver(request, func(event agentruntime.Event) bool { got = append(got, event); return true }))
	require.Len(t, got, 1)
	assert.IsType(t, &agentruntime.ErrorEvent{}, got[0], "unknown request cannot bypass bounded duplicate protection")
	assert.Len(t, history.elicitations, remoteHistoryMaxElicitations)
}

func TestRuntimeRootRecoveryPreservesDetachedOAuthRequests(t *testing.T) {
	t.Parallel()
	client := &elicitationRecordingClient{}
	rt, err := NewRuntime(client)
	require.NoError(t, err)
	rt.sessionID = "root"
	sub := &remoteEventSubscription{sessionID: "root", history: newRemoteMessageHistory(nil), cancel: func() {}}
	rt.background = sub
	var got []agentruntime.Event
	rt.OnBackgroundEvent(func(event agentruntime.Event) { got = append(got, event) })
	requests := []*agentruntime.ElicitationRequestEvent{
		{SessionID: "root", ElicitationID: "foreground"},
		{ElicitationID: "legacy-foreground"},
		{SessionID: "detached-a", ElicitationID: "oauth-a", ServerElicitationID: "wire-id"},
		{SessionID: "detached-b", ElicitationID: "oauth-b", ServerElicitationID: "wire-id"},
	}
	for _, request := range requests {
		request.Meta = map[string]any{"docker-agent/type": "oauth_flow"}
		require.True(t, rt.emitBackgroundEvent(sub, request))
	}
	snapshotClient := &recoveryBackend{snapshot: &api.SessionSnapshotResponse{ID: "root", LastEventSeq: 42}}
	_, err = rt.reconcileIdleSnapshot(t.Context(), sub, snapshotClient)
	require.NoError(t, err)
	require.Len(t, got, 5)
	assert.IsType(t, &agentruntime.SessionRecoveredEvent{}, got[4])
	assert.NotContains(t, rt.pendingOAuthElicitations, "foreground")
	assert.NotContains(t, rt.pendingOAuthElicitations, "legacy-foreground")
	require.Len(t, rt.pendingOAuthElicitations, 2, "root idleness does not complete detached requests")
	assert.Same(t, requests[2], rt.pendingOAuthElicitations["oauth-a"])
	assert.Same(t, requests[3], rt.pendingOAuthElicitations["oauth-b"])

	require.ErrorContains(t, rt.ResumeElicitation(t.Context(), tools.ElicitationActionAccept, nil), "explicit")
	require.NoError(t, rt.ResumeElicitation(t.Context(), tools.ElicitationActionAccept, nil, "form"))
	// Missing metadata fails before browser/network work, proving the OAuth path is still selected.
	require.ErrorContains(t, rt.ResumeElicitation(t.Context(), tools.ElicitationActionAccept, nil, "oauth-a"), "server_url")
	assert.Contains(t, rt.pendingOAuthElicitations, "oauth-b")
	require.NoError(t, rt.ResumeElicitation(t.Context(), tools.ElicitationActionDecline, nil, "oauth-b"))
	assert.Equal(t, []string{"form", "oauth-a", "oauth-b"}, client.ids)
	assert.Equal(t, []tools.ElicitationAction{tools.ElicitationActionAccept, tools.ElicitationActionDecline, tools.ElicitationActionDecline}, client.actions)
	assert.Empty(t, rt.pendingOAuthElicitations)
}

func TestRemoteMessageHistoryBoundsUnmatchedStreamStarts(t *testing.T) {
	t.Parallel()
	history := newRemoteMessageHistory(nil)
	for range remoteHistoryMaxMessages + 2 {
		require.True(t, history.deliver(agentruntime.StreamStarted("s", "root"), func(agentruntime.Event) bool { return true }))
	}
	assert.Empty(t, history.streamDepth)
	require.ErrorContains(t, history.reconcile(&api.SessionSnapshotResponse{ID: "s"}, func(agentruntime.Event) {
		t.Fatal("overflowed history must not replay messages")
	}), "retention limit")
}
