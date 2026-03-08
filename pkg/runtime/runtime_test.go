package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/cagent/pkg/agent"
	"github.com/docker/cagent/pkg/chat"
	"github.com/docker/cagent/pkg/config/latest"
	"github.com/docker/cagent/pkg/model/provider/base"
	"github.com/docker/cagent/pkg/modelsdev"
	"github.com/docker/cagent/pkg/permissions"
	"github.com/docker/cagent/pkg/rag"
	"github.com/docker/cagent/pkg/rag/database"
	"github.com/docker/cagent/pkg/rag/strategy"
	ragtypes "github.com/docker/cagent/pkg/rag/types"
	"github.com/docker/cagent/pkg/session"
	"github.com/docker/cagent/pkg/team"
	"github.com/docker/cagent/pkg/tools"
)

type stubToolSet struct {
	startErr error
	tools    []tools.Tool
	listErr  error
}

// Verify interface compliance
var (
	_ tools.ToolSet   = (*stubToolSet)(nil)
	_ tools.Startable = (*stubToolSet)(nil)
)

func newStubToolSet(startErr error, toolsList []tools.Tool, listErr error) tools.ToolSet {
	return &stubToolSet{
		startErr: startErr,
		tools:    toolsList,
		listErr:  listErr,
	}
}

func (s *stubToolSet) Start(context.Context) error { return s.startErr }
func (s *stubToolSet) Stop(context.Context) error  { return nil }
func (s *stubToolSet) Tools(context.Context) ([]tools.Tool, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.tools, nil
}

type mockStream struct {
	responses []chat.MessageStreamResponse
	idx       int
	closed    bool
}

func (m *mockStream) Recv() (chat.MessageStreamResponse, error) {
	if m.idx >= len(m.responses) {
		return chat.MessageStreamResponse{}, io.EOF
	}
	resp := m.responses[m.idx]
	m.idx++
	return resp, nil
}

func (m *mockStream) Close() { m.closed = true }

type streamBuilder struct{ responses []chat.MessageStreamResponse }

func newStreamBuilder() *streamBuilder {
	return &streamBuilder{responses: []chat.MessageStreamResponse{}}
}

func (b *streamBuilder) AddContent(content string) *streamBuilder {
	b.responses = append(b.responses, chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{
			Index: 0,
			Delta: chat.MessageDelta{Content: content},
		}},
	})
	return b
}

func (b *streamBuilder) AddReasoning(content string) *streamBuilder {
	b.responses = append(b.responses, chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{
			Index: 0,
			Delta: chat.MessageDelta{ReasoningContent: content},
		}},
	})
	return b
}

func (b *streamBuilder) AddToolCallName(id, name string) *streamBuilder {
	b.responses = append(b.responses, chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{
			Index: 0,
			Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{
				ID:       id,
				Type:     "function",
				Function: tools.FunctionCall{Name: name},
			}}},
		}},
	})
	return b
}

func (b *streamBuilder) AddToolCallArguments(id, argsChunk string) *streamBuilder {
	b.responses = append(b.responses, chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{
			Index: 0,
			Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{
				ID:       id,
				Type:     "function",
				Function: tools.FunctionCall{Arguments: argsChunk},
			}}},
		}},
	})
	return b
}

func (b *streamBuilder) AddStopWithUsage(input, output int64) *streamBuilder {
	b.responses = append(b.responses, chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{
			Index:        0,
			FinishReason: chat.FinishReasonStop,
		}},
		Usage: &chat.Usage{InputTokens: input, OutputTokens: output},
	})
	return b
}

func (b *streamBuilder) Build() *mockStream { return &mockStream{responses: b.responses} }

type mockProvider struct {
	id     string
	stream chat.MessageStream
}

func (m *mockProvider) ID() string { return m.id }

func (m *mockProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return m.stream, nil
}

func (m *mockProvider) BaseConfig() base.Config { return base.Config{} }

func (m *mockProvider) MaxTokens() int { return 0 }

type mockProviderWithError struct {
	id string
}

func (m *mockProviderWithError) ID() string { return m.id }

func (m *mockProviderWithError) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, fmt.Errorf("simulated error creating chat completion stream")
}

func (m *mockProviderWithError) BaseConfig() base.Config { return base.Config{} }

func (m *mockProviderWithError) MaxTokens() int { return 0 }

type mockModelStore struct {
	ModelStore
}

func (m mockModelStore) GetModel(_ context.Context, _ string) (*modelsdev.Model, error) {
	return nil, nil
}

func runSession(t *testing.T, sess *session.Session, stream *mockStream) []Event {
	t.Helper()

	prov := &mockProvider{id: "test/mock-model", stream: stream}
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess.Title = "Unit Test"

	evCh := rt.RunStream(t.Context(), sess)

	var events []Event
	for ev := range evCh {
		events = append(events, ev)
	}
	return events
}

func hasEventType(t *testing.T, events []Event, target Event) bool {
	t.Helper()

	want := reflect.TypeOf(target)
	for _, ev := range events {
		if reflect.TypeOf(ev) == want {
			return true
		}
	}
	return false
}

// assertEventsEqual compares two event slices, ignoring timestamps.
// Timestamps are inherently non-deterministic in tests.
func assertEventsEqual(t *testing.T, expected, actual []Event) {
	t.Helper()

	require.Len(t, actual, len(expected), "event count mismatch")

	for i := range expected {
		expectedType := reflect.TypeOf(expected[i])
		actualType := reflect.TypeOf(actual[i])
		assert.Equal(t, expectedType, actualType, "event type mismatch at index %d", i)

		// Clear timestamps for comparison
		clearTimestamps(expected[i])
		clearTimestamps(actual[i])

		assert.Equal(t, expected[i], actual[i], "event content mismatch at index %d", i)
	}
}

// clearTimestamps sets Timestamp fields to zero value in events for comparison.
func clearTimestamps(event Event) {
	if event == nil {
		return
	}

	// Use reflection to find and clear Timestamp in embedded AgentContext
	v := reflect.ValueOf(event)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return
	}

	field := v.FieldByName("AgentContext")
	if !field.IsValid() || field.Kind() != reflect.Struct {
		return
	}

	timestampField := field.FieldByName("Timestamp")
	if timestampField.IsValid() && timestampField.CanSet() {
		timestampField.Set(reflect.Zero(timestampField.Type()))
	}
}

func TestSimple(t *testing.T) {
	stream := newStreamBuilder().
		AddContent("Hello").
		AddStopWithUsage(3, 2).
		Build()

	sess := session.New(session.WithUserMessage("Hi"))

	events := runSession(t, sess, stream)

	// Extract the actual message from MessageAddedEvent to use in comparison
	// (it contains dynamic fields like CreatedAt that we can't predict)
	require.Len(t, events, 10)
	msgAdded := events[7].(*MessageAddedEvent)
	require.NotNil(t, msgAdded.Message)
	require.Equal(t, "Hello", msgAdded.Message.Message.Content)
	require.Equal(t, chat.MessageRoleAssistant, msgAdded.Message.Message.Role)

	expectedEvents := []Event{
		AgentInfo("root", "test/mock-model", "", ""),
		TeamInfo([]AgentDetails{{Name: "root", Provider: "test", Model: "mock-model"}}, "root"),
		ToolsetInfo(0, false, "root"),
		UserMessage("Hi", sess.ID, nil, 0),
		StreamStarted(sess.ID, "root"),
		ToolsetInfo(0, false, "root"),
		AgentChoice("root", "Hello"),
		MessageAdded(sess.ID, msgAdded.Message, "root"),
		NewTokenUsageEvent(sess.ID, "root", &Usage{InputTokens: 3, OutputTokens: 2, ContextLength: 5, LastMessage: &MessageUsage{
			Usage: chat.Usage{InputTokens: 3, OutputTokens: 2},
			Model: "test/mock-model",
		}}),
		StreamStopped(sess.ID, "root"),
	}

	assertEventsEqual(t, expectedEvents, events)
}

func TestMultipleContentChunks(t *testing.T) {
	stream := newStreamBuilder().
		AddContent("Hello ").
		AddContent("there, ").
		AddContent("how ").
		AddContent("are ").
		AddContent("you?").
		AddStopWithUsage(8, 12).
		Build()

	sess := session.New(session.WithUserMessage("Please greet me"))

	events := runSession(t, sess, stream)

	// Extract the actual message from MessageAddedEvent to use in comparison
	// (it contains dynamic fields like CreatedAt that we can't predict)
	require.Len(t, events, 14)
	msgAdded := events[11].(*MessageAddedEvent)
	require.NotNil(t, msgAdded.Message)

	expectedEvents := []Event{
		AgentInfo("root", "test/mock-model", "", ""),
		TeamInfo([]AgentDetails{{Name: "root", Provider: "test", Model: "mock-model"}}, "root"),
		ToolsetInfo(0, false, "root"),
		UserMessage("Please greet me", sess.ID, nil, 0),
		StreamStarted(sess.ID, "root"),
		ToolsetInfo(0, false, "root"),
		AgentChoice("root", "Hello "),
		AgentChoice("root", "there, "),
		AgentChoice("root", "how "),
		AgentChoice("root", "are "),
		AgentChoice("root", "you?"),
		MessageAdded(sess.ID, msgAdded.Message, "root"),
		NewTokenUsageEvent(sess.ID, "root", &Usage{InputTokens: 8, OutputTokens: 12, ContextLength: 20, LastMessage: &MessageUsage{
			Usage: chat.Usage{InputTokens: 8, OutputTokens: 12},
			Model: "test/mock-model",
		}}),
		StreamStopped(sess.ID, "root"),
	}

	assertEventsEqual(t, expectedEvents, events)
}

func TestWithReasoning(t *testing.T) {
	stream := newStreamBuilder().
		AddReasoning("Let me think about this...").
		AddReasoning(" I should respond politely.").
		AddContent("Hello, how can I help you?").
		AddStopWithUsage(10, 15).
		Build()

	sess := session.New(session.WithUserMessage("Hi"))

	events := runSession(t, sess, stream)

	// Extract the actual message from MessageAddedEvent to use in comparison
	// (it contains dynamic fields like CreatedAt that we can't predict)
	require.Len(t, events, 12)
	msgAdded := events[9].(*MessageAddedEvent)
	require.NotNil(t, msgAdded.Message)

	expectedEvents := []Event{
		AgentInfo("root", "test/mock-model", "", ""),
		TeamInfo([]AgentDetails{{Name: "root", Provider: "test", Model: "mock-model"}}, "root"),
		ToolsetInfo(0, false, "root"),
		UserMessage("Hi", sess.ID, nil, 0),
		StreamStarted(sess.ID, "root"),
		ToolsetInfo(0, false, "root"),
		AgentChoiceReasoning("root", "Let me think about this..."),
		AgentChoiceReasoning("root", " I should respond politely."),
		AgentChoice("root", "Hello, how can I help you?"),
		MessageAdded(sess.ID, msgAdded.Message, "root"),
		NewTokenUsageEvent(sess.ID, "root", &Usage{InputTokens: 10, OutputTokens: 15, ContextLength: 25, LastMessage: &MessageUsage{
			Usage: chat.Usage{InputTokens: 10, OutputTokens: 15},
			Model: "test/mock-model",
		}}),
		StreamStopped(sess.ID, "root"),
	}

	assertEventsEqual(t, expectedEvents, events)
}

func TestMixedContentAndReasoning(t *testing.T) {
	stream := newStreamBuilder().
		AddReasoning("The user wants a greeting").
		AddContent("Hello!").
		AddReasoning(" I should be friendly").
		AddContent(" How can I help you today?").
		AddStopWithUsage(15, 20).
		Build()

	sess := session.New(session.WithUserMessage("Hi there"))

	events := runSession(t, sess, stream)

	// Extract the actual message from MessageAddedEvent to use in comparison
	// (it contains dynamic fields like CreatedAt that we can't predict)
	require.Len(t, events, 13)
	msgAdded := events[10].(*MessageAddedEvent)
	require.NotNil(t, msgAdded.Message)

	expectedEvents := []Event{
		AgentInfo("root", "test/mock-model", "", ""),
		TeamInfo([]AgentDetails{{Name: "root", Provider: "test", Model: "mock-model"}}, "root"),
		ToolsetInfo(0, false, "root"),
		UserMessage("Hi there", sess.ID, nil, 0),
		StreamStarted(sess.ID, "root"),
		ToolsetInfo(0, false, "root"),
		AgentChoiceReasoning("root", "The user wants a greeting"),
		AgentChoice("root", "Hello!"),
		AgentChoiceReasoning("root", " I should be friendly"),
		AgentChoice("root", " How can I help you today?"),
		MessageAdded(sess.ID, msgAdded.Message, "root"),
		NewTokenUsageEvent(sess.ID, "root", &Usage{InputTokens: 15, OutputTokens: 20, ContextLength: 35, LastMessage: &MessageUsage{
			Usage: chat.Usage{InputTokens: 15, OutputTokens: 20},
			Model: "test/mock-model",
		}}),
		StreamStopped(sess.ID, "root"),
	}

	assertEventsEqual(t, expectedEvents, events)
}

func TestToolCallSequence(t *testing.T) {
	stream := newStreamBuilder().
		AddToolCallName("call_123", "test_tool").
		AddToolCallArguments("call_123", `{"param": "value"}`).
		AddStopWithUsage(5, 8).
		Build()

	sess := session.New(session.WithUserMessage("Please use the test tool"))

	events := runSession(t, sess, stream)

	require.True(t, hasEventType(t, events, &PartialToolCallEvent{}), "Expected PartialToolCallEvent")
	require.False(t, hasEventType(t, events, &ToolCallEvent{}), "Should not have ToolCallEvent without actual tool execution")

	require.True(t, hasEventType(t, events, &StreamStartedEvent{}), "Expected StreamStartedEvent")
	require.True(t, hasEventType(t, events, &StreamStoppedEvent{}), "Expected StreamStoppedEvent")
}

func TestErrorEvent(t *testing.T) {
	prov := &mockProviderWithError{id: "test/error-model"}
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Hi"))
	sess.Title = "Unit Test"

	evCh := rt.RunStream(t.Context(), sess)

	var events []Event
	for ev := range evCh {
		events = append(events, ev)
	}

	require.Len(t, events, 8)
	require.IsType(t, &AgentInfoEvent{}, events[0])
	require.IsType(t, &TeamInfoEvent{}, events[1])
	require.IsType(t, &ToolsetInfoEvent{}, events[2])
	require.IsType(t, &UserMessageEvent{}, events[3])
	require.IsType(t, &StreamStartedEvent{}, events[4])
	require.IsType(t, &ToolsetInfoEvent{}, events[5])
	require.IsType(t, &ErrorEvent{}, events[6])
	require.IsType(t, &StreamStoppedEvent{}, events[7])

	errorEvent := events[6].(*ErrorEvent)
	require.Contains(t, errorEvent.Error, "simulated error")
}

func TestContextCancellation(t *testing.T) {
	stream := newStreamBuilder().
		AddContent("This should not complete").
		AddStopWithUsage(10, 5).
		Build()

	prov := &mockProvider{id: "test/mock-model", stream: stream}
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Hi"))
	sess.Title = "Unit Test"

	ctx, cancel := context.WithCancel(t.Context())
	evCh := rt.RunStream(ctx, sess)

	cancel()

	var events []Event
	for ev := range evCh {
		events = append(events, ev)
	}

	require.GreaterOrEqual(t, len(events), 5)
	require.IsType(t, &AgentInfoEvent{}, events[0])
	require.IsType(t, &TeamInfoEvent{}, events[1])
	require.IsType(t, &ToolsetInfoEvent{}, events[2])
	require.IsType(t, &UserMessageEvent{}, events[3])
	require.IsType(t, &StreamStartedEvent{}, events[4])
	require.IsType(t, &StreamStoppedEvent{}, events[len(events)-1])
}

// stubRAGStrategy is a minimal implementation of strategy.Strategy for testing RAG initialization.
type stubRAGStrategy struct{}

func (s *stubRAGStrategy) Initialize(_ context.Context, _ []string, _ strategy.ChunkingConfig) error {
	return nil
}

func (s *stubRAGStrategy) Query(_ context.Context, _ string, _ int, _ float64) ([]database.SearchResult, error) {
	return nil, nil
}

func (s *stubRAGStrategy) CheckAndReindexChangedFiles(_ context.Context, _ []string, _ strategy.ChunkingConfig) error {
	return nil
}

func (s *stubRAGStrategy) StartFileWatcher(_ context.Context, _ []string, _ strategy.ChunkingConfig) error {
	return nil
}

func (s *stubRAGStrategy) Close() error { return nil }

func TestStartBackgroundRAGInit_StopsForwardingAfterContextCancel(t *testing.T) {
	t.Parallel()

	baseCtx := t.Context()
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()

	// Build a RAG manager with a stub strategy and a controllable event channel.
	strategyEvents := make(chan ragtypes.Event, 10)
	mgr, err := rag.New(
		ctx,
		"test-rag",
		rag.Config{
			StrategyConfigs: []strategy.Config{
				{
					Name:     "stub",
					Strategy: &stubRAGStrategy{},
					Docs:     nil,
				},
			},
		},
		strategyEvents,
	)
	require.NoError(t, err)
	defer func() {
		_ = mgr.Close()
	}()

	tm := team.New(team.WithRAGManagers(map[string]*rag.Manager{
		"default": mgr,
	}))

	rt := &LocalRuntime{
		team:         tm,
		currentAgent: "root",
	}

	eventsCh := make(chan Event, 10)

	// Start background RAG init with event forwarding.
	rt.StartBackgroundRAGInit(ctx, func(ev Event) {
		eventsCh <- ev
	})

	// Emit an "indexing_completed" event and ensure it is forwarded.
	strategyEvents <- ragtypes.Event{
		Type:         ragtypes.EventTypeIndexingComplete,
		StrategyName: "stub",
	}

	select {
	case <-eventsCh:
		// ok: at least one event forwarded
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("expected RAG event to be forwarded before cancellation")
	}

	// Cancel the context and ensure no further events are forwarded.
	cancel()

	// Brief yield to allow the forwarder goroutine to observe cancellation.
	// This is a timing-based negative test: we verify no event is forwarded.
	time.Sleep(10 * time.Millisecond)

	// Emit another event; it should NOT be forwarded.
	strategyEvents <- ragtypes.Event{
		Type:         ragtypes.EventTypeIndexingComplete,
		StrategyName: "stub",
	}

	select {
	case ev := <-eventsCh:
		t.Fatalf("expected no events after cancellation, got %T", ev)
	case <-time.After(20 * time.Millisecond):
		// success: no events forwarded
	}
}

func TestToolCallVariations(t *testing.T) {
	tests := []struct {
		name          string
		streamBuilder func() *streamBuilder
		description   string
	}{
		{
			name: "tool_call_with_empty_args",
			streamBuilder: func() *streamBuilder {
				return newStreamBuilder().
					AddToolCallName("call_1", "empty_tool").
					AddToolCallArguments("call_1", "{}").
					AddStopWithUsage(3, 5)
			},
			description: "Tool call with empty JSON arguments",
		},
		{
			name: "multiple_tool_calls",
			streamBuilder: func() *streamBuilder {
				return newStreamBuilder().
					AddToolCallName("call_1", "tool_one").
					AddToolCallArguments("call_1", `{"param":"value1"}`).
					AddToolCallName("call_2", "tool_two").
					AddToolCallArguments("call_2", `{"param":"value2"}`).
					AddStopWithUsage(8, 12)
			},
			description: "Multiple tool calls in sequence",
		},
		{
			name: "tool_call_with_fragmented_args",
			streamBuilder: func() *streamBuilder {
				return newStreamBuilder().
					AddToolCallName("call_1", "fragmented_tool").
					AddToolCallArguments("call_1", `{"long`).
					AddToolCallArguments("call_1", `_param": "`).
					AddToolCallArguments("call_1", `some_value"}`).
					AddStopWithUsage(5, 8)
			},
			description: "Tool call with arguments streamed in fragments",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := tt.streamBuilder().Build()
			sess := session.New(session.WithUserMessage("Use tools"))
			events := runSession(t, sess, stream)

			require.True(t, hasEventType(t, events, &PartialToolCallEvent{}), "Expected PartialToolCallEvent for %s", tt.description)
			require.True(t, hasEventType(t, events, &StreamStartedEvent{}), "Expected StreamStartedEvent")
			require.True(t, hasEventType(t, events, &StreamStoppedEvent{}), "Expected StreamStoppedEvent")
		})
	}
}

// queueProvider returns a different stream on each CreateChatCompletionStream call.
type queueProvider struct {
	id      string
	mu      sync.Mutex
	streams []chat.MessageStream
}

func (p *queueProvider) ID() string { return p.id }

func (p *queueProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.streams) == 0 {
		return &mockStream{}, nil
	}
	s := p.streams[0]
	p.streams = p.streams[1:]
	return s, nil
}

func (p *queueProvider) BaseConfig() base.Config { return base.Config{} }

func (p *queueProvider) MaxTokens() int { return 0 }

type mockModelStoreWithLimit struct {
	ModelStore
	limit int
}

func (m mockModelStoreWithLimit) GetModel(_ context.Context, _ string) (*modelsdev.Model, error) {
	return &modelsdev.Model{Limit: modelsdev.Limit{Context: m.limit}, Cost: &modelsdev.Cost{}}, nil
}

func TestCompaction(t *testing.T) {
	// First stream: assistant issues a tool call and usage exceeds 90% threshold
	mainStream := newStreamBuilder().
		AddContent("Hello there").
		AddStopWithUsage(101, 0). // Context limit will be 100
		Build()

	// Second stream: summary generation (simple content)
	summaryStream := newStreamBuilder().
		AddContent("summary").
		AddStopWithUsage(1, 1).
		Build()

	prov := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{mainStream, summaryStream}}

	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	// Enable compaction and provide a model store with context limit = 100
	rt, err := NewLocalRuntime(tm, WithSessionCompaction(true), WithModelStore(mockModelStoreWithLimit{limit: 100}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Start"))
	e := rt.RunStream(t.Context(), sess)
	for range e {
	}
	sess.AddMessage(session.UserMessage("Again"))
	events := rt.RunStream(t.Context(), sess)

	var seen []Event
	for ev := range events {
		seen = append(seen, ev)
	}

	compactionStartIdx := -1
	for i, ev := range seen {
		if e, ok := ev.(*SessionCompactionEvent); ok {
			if e.Status == "started" && compactionStartIdx == -1 {
				compactionStartIdx = i
			}
		}
	}

	require.NotEqual(t, -1, compactionStartIdx, "expected a SessionCompaction start event")
}

func TestSessionWithoutUserMessage(t *testing.T) {
	stream := newStreamBuilder().AddContent("OK").AddStopWithUsage(1, 1).Build()

	sess := session.New(
		session.WithSendUserMessage(false),
	)

	events := runSession(t, sess, stream)

	require.True(t, hasEventType(t, events, &StreamStartedEvent{}), "Expected StreamStartedEvent")
	require.True(t, hasEventType(t, events, &StreamStoppedEvent{}), "Expected StreamStoppedEvent")
	require.False(t, hasEventType(t, events, &UserMessageEvent{}), "Should not have UserMessageEvent when SendUserMessage is false")
}

// --- Tool setup failure handling tests ---

func collectEvents(ch chan Event) []Event {
	n := len(ch)
	evs := make([]Event, 0, n)
	for range n {
		evs = append(evs, <-ch)
	}
	return evs
}

func hasWarningEvent(evs []Event) bool {
	for _, e := range evs {
		if _, ok := e.(*WarningEvent); ok {
			return true
		}
	}
	return false
}

func TestGetTools_WarningHandling(t *testing.T) {
	tests := []struct {
		name          string
		toolsets      []tools.ToolSet
		wantToolCount int
		wantWarning   bool
	}{
		{
			name:          "partial success warns once",
			toolsets:      []tools.ToolSet{newStubToolSet(nil, []tools.Tool{{Name: "good", Parameters: map[string]any{}}}, nil), newStubToolSet(errors.New("boom"), nil, nil)},
			wantToolCount: 1,
			wantWarning:   true,
		},
		{
			name:          "all fail on start warns once",
			toolsets:      []tools.ToolSet{newStubToolSet(errors.New("s1"), nil, nil), newStubToolSet(errors.New("s2"), nil, nil)},
			wantToolCount: 0,
			wantWarning:   true,
		},
		{
			name:          "list failure warns once",
			toolsets:      []tools.ToolSet{newStubToolSet(nil, nil, errors.New("boom"))},
			wantToolCount: 0,
			wantWarning:   true,
		},
		{
			name:          "no toolsets no warning",
			toolsets:      nil,
			wantToolCount: 0,
			wantWarning:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := agent.New("root", "test", agent.WithToolSets(tt.toolsets...), agent.WithModel(&mockProvider{}))
			tm := team.New(team.WithAgents(root))
			rt, err := NewLocalRuntime(tm, WithModelStore(mockModelStore{}))
			require.NoError(t, err)

			events := make(chan Event, 10)
			sessionSpan := trace.SpanFromContext(t.Context())

			// First call
			tools1, err := rt.getTools(t.Context(), root, sessionSpan, events)
			require.NoError(t, err)
			require.Len(t, tools1, tt.wantToolCount)

			rt.emitAgentWarnings(root, events)
			evs := collectEvents(events)
			require.Equal(t, tt.wantWarning, hasWarningEvent(evs), "warning event mismatch on first call")
		})
	}
}

func TestNewRuntime_NoAgentsError(t *testing.T) {
	tm := team.New()

	_, err := New(tm, WithModelStore(mockModelStore{}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "no agents loaded")
}

func TestNewRuntime_InvalidCurrentAgentError(t *testing.T) {
	root := agent.New("root", "You are a test agent")
	tm := team.New(team.WithAgents(root))

	// Ask for a non-existent current agent
	_, err := New(tm, WithCurrentAgent("other"), WithModelStore(mockModelStore{}))
	require.Contains(t, err.Error(), "agent not found: other (available agents: root)")
}

func TestSummarize_EmptySession(t *testing.T) {
	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New()
	sess.Title = "Empty Session Test"

	// Try to summarize the empty session
	events := make(chan Event, 10)
	rt.Summarize(t.Context(), sess, "", events)
	close(events)

	// Collect events
	var warningFound bool
	var warningMsg string
	for ev := range events {
		if warningEvent, ok := ev.(*WarningEvent); ok {
			warningFound = true
			warningMsg = warningEvent.Message
		}
	}

	// Should have received a warning event about empty session
	require.True(t, warningFound, "expected a warning event for empty session")
	require.Contains(t, warningMsg, "empty", "warning message should mention empty session")
}

func TestProcessToolCalls_UnknownTool_ReturnsErrorResponse(t *testing.T) {
	root := agent.New("root", "You are a test agent", agent.WithModel(&mockProvider{}))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	rt.registerDefaultTools()

	sess := session.New(session.WithUserMessage("Start"))

	calls := []tools.ToolCall{{
		ID:       "tool-unknown-1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "non_existent_tool", Arguments: "{}"},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, nil, events)
	close(events)
	for range events {
	}

	// The model must receive an error tool response so it can self-correct.
	var toolContent string
	for _, it := range sess.Messages {
		if it.IsMessage() && it.Message.Message.Role == chat.MessageRoleTool && it.Message.Message.ToolCallID == "tool-unknown-1" {
			toolContent = it.Message.Message.Content
		}
	}
	require.NotEmpty(t, toolContent, "expected an error tool response for unknown tools")
	assert.Contains(t, toolContent, "not available")
}

func TestEmitStartupInfo(t *testing.T) {
	// Create a simple agent with mock provider
	prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}
	root := agent.New("startup-test-agent", "You are a startup test agent",
		agent.WithModel(prov),
		agent.WithDescription("This is a startup test agent"),
		agent.WithWelcomeMessage("Welcome!"),
	)
	other := agent.New("other-agent", "You are another agent",
		agent.WithModel(prov),
		agent.WithDescription("This is another agent"),
	)
	tm := team.New(team.WithAgents(root, other))

	rt, err := NewLocalRuntime(tm, WithCurrentAgent("startup-test-agent"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// Create a channel to collect events
	events := make(chan Event, 10)

	// Call EmitStartupInfo
	rt.EmitStartupInfo(t.Context(), nil, events)
	close(events)

	// Collect events
	var collectedEvents []Event
	for event := range events {
		collectedEvents = append(collectedEvents, event)
	}

	// Verify expected events are emitted
	expectedEvents := []Event{
		AgentInfo("startup-test-agent", "test/startup-model", "This is a startup test agent", "Welcome!"),
		TeamInfo([]AgentDetails{
			{Name: "startup-test-agent", Description: "This is a startup test agent", Provider: "test", Model: "startup-model"},
			{Name: "other-agent", Description: "This is another agent", Provider: "test", Model: "startup-model"},
		}, "startup-test-agent"),
		ToolsetInfo(0, false, "startup-test-agent"), // No tools configured
	}

	assertEventsEqual(t, expectedEvents, collectedEvents)

	// Test that calling EmitStartupInfo again doesn't emit duplicate events
	events2 := make(chan Event, 10)
	rt.EmitStartupInfo(t.Context(), nil, events2)
	close(events2)

	var collectedEvents2 []Event
	for event := range events2 {
		collectedEvents2 = append(collectedEvents2, event)
	}

	// Should be empty due to deduplication
	require.Empty(t, collectedEvents2, "EmitStartupInfo should not emit duplicate events")
}

func TestEmitStartupInfo_WithSessionTokenData(t *testing.T) {
	// When restoring a session that already has token data,
	// EmitStartupInfo should emit a TokenUsageEvent with the context limit
	// looked up from the model store so the sidebar can display context %.
	prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}
	root := agent.New("startup-test-agent", "You are a startup test agent",
		agent.WithModel(prov),
		agent.WithDescription("Startup agent"),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm, WithCurrentAgent("startup-test-agent"),
		WithModelStore(mockModelStoreWithLimit{limit: 200_000}))
	require.NoError(t, err)

	// Create a session with existing token data (simulating session restore)
	sess := session.New()
	sess.InputTokens = 5000
	sess.OutputTokens = 1000

	events := make(chan Event, 20)
	rt.EmitStartupInfo(t.Context(), sess, events)
	close(events)

	// Collect events and find the TokenUsageEvent
	var tokenEvent *TokenUsageEvent
	for event := range events {
		if te, ok := event.(*TokenUsageEvent); ok {
			tokenEvent = te
		}
	}

	require.NotNil(t, tokenEvent, "EmitStartupInfo should emit a TokenUsageEvent for a session with token data")
	assert.Equal(t, sess.ID, tokenEvent.SessionID)
	assert.Equal(t, int64(5000), tokenEvent.Usage.InputTokens)
	assert.Equal(t, int64(1000), tokenEvent.Usage.OutputTokens)
	assert.Equal(t, int64(6000), tokenEvent.Usage.ContextLength)
	assert.Equal(t, int64(200_000), tokenEvent.Usage.ContextLimit)
}

func TestEmitStartupInfo_CostIncludesSubSessions(t *testing.T) {
	// When restoring a branched session that contains sub-sessions,
	// the emitted TokenUsageEvent.Cost must include sub-session costs
	// (TotalCost), not just OwnCost, because sub-sessions won't emit
	// their own events during restore.
	prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}
	root := agent.New("root", "agent",
		agent.WithModel(prov),
		agent.WithDescription("Root"),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm, WithCurrentAgent("root"),
		WithModelStore(mockModelStoreWithLimit{limit: 128_000}))
	require.NoError(t, err)

	// Build a session with a direct message and a sub-session.
	sess := session.New()
	sess.InputTokens = 1000
	sess.OutputTokens = 500

	// Direct assistant message with cost
	sess.Messages = append(sess.Messages, session.Item{
		Message: &session.Message{
			AgentName: "root",
			Message: chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "hello",
				Cost:    0.01,
				Usage:   &chat.Usage{InputTokens: 800, OutputTokens: 400},
			},
		},
	})

	// Sub-session with its own cost
	subSess := session.New()
	subSess.Messages = append(subSess.Messages, session.Item{
		Message: &session.Message{
			AgentName: "sub",
			Message: chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "sub response",
				Cost:    0.05,
				Usage:   &chat.Usage{InputTokens: 200, OutputTokens: 100},
			},
		},
	})
	sess.Messages = append(sess.Messages, session.Item{SubSession: subSess})

	events := make(chan Event, 20)
	rt.EmitStartupInfo(t.Context(), sess, events)
	close(events)

	var tokenEvent *TokenUsageEvent
	for event := range events {
		if te, ok := event.(*TokenUsageEvent); ok {
			tokenEvent = te
		}
	}

	require.NotNil(t, tokenEvent, "should emit TokenUsageEvent")
	// Cost must equal TotalCost (0.01 + 0.05 = 0.06), not OwnCost (0.01).
	assert.InDelta(t, 0.06, tokenEvent.Usage.Cost, 0.0001,
		"cost should include sub-session costs (TotalCost, not OwnCost)")
}

func TestEmitStartupInfo_NilSessionNoTokenEvent(t *testing.T) {
	// When sess is nil, no TokenUsageEvent should be emitted.
	prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}
	root := agent.New("startup-test-agent", "You are a startup test agent",
		agent.WithModel(prov),
		agent.WithDescription("Startup agent"),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm, WithCurrentAgent("startup-test-agent"),
		WithModelStore(mockModelStoreWithLimit{limit: 200_000}))
	require.NoError(t, err)

	events := make(chan Event, 20)
	rt.EmitStartupInfo(t.Context(), nil, events)
	close(events)

	for event := range events {
		_, isTokenEvent := event.(*TokenUsageEvent)
		assert.False(t, isTokenEvent, "EmitStartupInfo should not emit TokenUsageEvent when session is nil")
	}
}

func TestPermissions_DenyBlocksToolExecution(t *testing.T) {
	// Test that tools matching deny patterns are blocked
	permChecker := permissions.NewChecker(&latest.PermissionsConfig{
		Deny: []string{"dangerous_tool"},
	})

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(
		team.WithAgents(root),
		team.WithPermissions(permChecker),
	)

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Test"))

	// Create a tool call for the denied tool
	calls := []tools.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "dangerous_tool", Arguments: "{}"},
	}}

	// Define a tool that exists
	agentTools := []tools.Tool{{
		Name:       "dangerous_tool",
		Parameters: map[string]any{},
		Handler: func(ctx context.Context, tc tools.ToolCall) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("executed"), nil
		},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, agentTools, events)
	close(events)

	// The tool should be denied, look for a ToolCallResponseEvent with error
	var toolResponse *ToolCallResponseEvent
	for ev := range events {
		if tr, ok := ev.(*ToolCallResponseEvent); ok {
			toolResponse = tr
			break
		}
	}

	require.NotNil(t, toolResponse, "expected ToolCallResponseEvent")
	require.Contains(t, toolResponse.Response, "denied by permissions")
}

func TestPermissions_AllowAutoApprovesTool(t *testing.T) {
	// Test that tools matching allow patterns are auto-approved without --yolo
	permChecker := permissions.NewChecker(&latest.PermissionsConfig{
		Allow: []string{"safe_*"},
	})

	var executed bool
	agentTools := []tools.Tool{{
		Name:       "safe_tool",
		Parameters: map[string]any{},
		Handler: func(ctx context.Context, tc tools.ToolCall) (*tools.ToolCallResult, error) {
			executed = true
			return tools.ResultSuccess("executed"), nil
		},
	}}

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "You are a test agent",
		agent.WithModel(prov),
		agent.WithToolSets(newStubToolSet(nil, agentTools, nil)),
	)
	tm := team.New(
		team.WithAgents(root),
		team.WithPermissions(permChecker),
	)

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Test"))
	// Note: ToolsApproved is false (no --yolo)
	require.False(t, sess.ToolsApproved)

	calls := []tools.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "safe_tool", Arguments: "{}"},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, agentTools, events)
	close(events)

	// The tool should have been executed due to allow pattern
	require.True(t, executed, "expected tool to be auto-approved and executed")
}

func TestPermissions_DenyTakesPriorityOverAllow(t *testing.T) {
	// Test that deny patterns take priority over allow patterns
	permChecker := permissions.NewChecker(&latest.PermissionsConfig{
		Allow: []string{"*"}, // Allow everything
		Deny:  []string{"forbidden_tool"},
	})

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(
		team.WithAgents(root),
		team.WithPermissions(permChecker),
	)

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Test"))

	calls := []tools.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "forbidden_tool", Arguments: "{}"},
	}}

	agentTools := []tools.Tool{{
		Name:       "forbidden_tool",
		Parameters: map[string]any{},
		Handler: func(ctx context.Context, tc tools.ToolCall) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("executed"), nil
		},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, agentTools, events)
	close(events)

	// The tool should be denied despite wildcard allow
	var toolResponse *ToolCallResponseEvent
	for ev := range events {
		if tr, ok := ev.(*ToolCallResponseEvent); ok {
			toolResponse = tr
			break
		}
	}

	require.NotNil(t, toolResponse, "expected ToolCallResponseEvent")
	require.Contains(t, toolResponse.Response, "denied by permissions")
}

func TestSessionPermissions_DenyBlocksToolExecution(t *testing.T) {
	// Test that session-level deny patterns block tools
	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// Create session with permissions that deny the tool
	sess := session.New(
		session.WithUserMessage("Test"),
		session.WithPermissions(&session.PermissionsConfig{
			Deny: []string{"blocked_tool"},
		}),
	)

	calls := []tools.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "blocked_tool", Arguments: "{}"},
	}}

	agentTools := []tools.Tool{{
		Name:       "blocked_tool",
		Parameters: map[string]any{},
		Handler: func(ctx context.Context, tc tools.ToolCall) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("executed"), nil
		},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, agentTools, events)
	close(events)

	var toolResponse *ToolCallResponseEvent
	for ev := range events {
		if tr, ok := ev.(*ToolCallResponseEvent); ok {
			toolResponse = tr
			break
		}
	}

	require.NotNil(t, toolResponse, "expected ToolCallResponseEvent")
	require.Contains(t, toolResponse.Response, "denied by session permissions")
}

func TestSessionPermissions_AllowAutoApprovesTool(t *testing.T) {
	// Test that session-level allow patterns auto-approve tools
	var executed bool
	agentTools := []tools.Tool{{
		Name:       "allowed_tool",
		Parameters: map[string]any{},
		Handler: func(ctx context.Context, tc tools.ToolCall) (*tools.ToolCallResult, error) {
			executed = true
			return tools.ResultSuccess("executed"), nil
		},
	}}

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "You are a test agent",
		agent.WithModel(prov),
		agent.WithToolSets(newStubToolSet(nil, agentTools, nil)),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// Create session with permissions that allow the tool
	sess := session.New(
		session.WithUserMessage("Test"),
		session.WithPermissions(&session.PermissionsConfig{
			Allow: []string{"allowed_*"},
		}),
	)
	require.False(t, sess.ToolsApproved) // No --yolo

	calls := []tools.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "allowed_tool", Arguments: "{}"},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, agentTools, events)
	close(events)

	require.True(t, executed, "expected tool to be auto-approved by session permissions")
}

func TestSessionPermissions_TakePriorityOverTeamPermissions(t *testing.T) {
	// Test that session permissions are evaluated before team permissions
	// Team allows everything, but session denies specific tool
	teamPermChecker := permissions.NewChecker(&latest.PermissionsConfig{
		Allow: []string{"*"}, // Team allows all
	})

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(
		team.WithAgents(root),
		team.WithPermissions(teamPermChecker),
	)

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// Session denies the tool (should override team allow)
	sess := session.New(
		session.WithUserMessage("Test"),
		session.WithPermissions(&session.PermissionsConfig{
			Deny: []string{"overridden_tool"},
		}),
	)

	calls := []tools.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "overridden_tool", Arguments: "{}"},
	}}

	agentTools := []tools.Tool{{
		Name:       "overridden_tool",
		Parameters: map[string]any{},
		Handler: func(ctx context.Context, tc tools.ToolCall) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("executed"), nil
		},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, agentTools, events)
	close(events)

	// Session deny should take priority over team allow
	var toolResponse *ToolCallResponseEvent
	for ev := range events {
		if tr, ok := ev.(*ToolCallResponseEvent); ok {
			toolResponse = tr
			break
		}
	}

	require.NotNil(t, toolResponse, "expected ToolCallResponseEvent")
	require.Contains(t, toolResponse.Response, "denied by session permissions")
}

func TestToolRejectionWithReason(t *testing.T) {
	// Test that rejection reasons are included in the tool error response
	agentTools := []tools.Tool{{
		Name:       "shell",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolCallResult, error) {
			t.Fatal("tool should not be executed when rejected")
			return nil, nil
		},
	}}

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "You are a test agent",
		agent.WithModel(prov),
		agent.WithToolSets(newStubToolSet(nil, agentTools, nil)),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Test"))
	require.False(t, sess.ToolsApproved) // No --yolo

	calls := []tools.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "shell", Arguments: "{}"},
	}}

	events := make(chan Event, 10)

	// Run in goroutine since it will block waiting for confirmation
	go func() {
		rt.processToolCalls(t.Context(), sess, calls, agentTools, events)
		close(events)
	}()

	// Wait for confirmation request and then reject with a reason
	var toolResponse *ToolCallResponseEvent
	for ev := range events {
		if _, ok := ev.(*ToolCallConfirmationEvent); ok {
			// Send rejection with a specific reason
			rt.resumeChan <- ResumeReject("The arguments provided are incorrect.")
		}
		if resp, ok := ev.(*ToolCallResponseEvent); ok {
			toolResponse = resp
		}
	}

	require.NotNil(t, toolResponse, "expected a tool response event")
	require.True(t, toolResponse.Result.IsError, "expected tool result to be an error")
	require.Contains(t, toolResponse.Response, "The user rejected the tool call.")
	require.Contains(t, toolResponse.Response, "Reason: The arguments provided are incorrect.")
}

func TestToolRejectionWithoutReason(t *testing.T) {
	// Test that rejection without a reason still works
	agentTools := []tools.Tool{{
		Name:       "shell",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolCallResult, error) {
			t.Fatal("tool should not be executed when rejected")
			return nil, nil
		},
	}}

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "You are a test agent",
		agent.WithModel(prov),
		agent.WithToolSets(newStubToolSet(nil, agentTools, nil)),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Test"))
	require.False(t, sess.ToolsApproved) // No --yolo

	calls := []tools.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "shell", Arguments: "{}"},
	}}

	events := make(chan Event, 10)

	// Run in goroutine since it will block waiting for confirmation
	go func() {
		rt.processToolCalls(t.Context(), sess, calls, agentTools, events)
		close(events)
	}()

	// Wait for confirmation request and then reject without a reason
	var toolResponse *ToolCallResponseEvent
	for ev := range events {
		if _, ok := ev.(*ToolCallConfirmationEvent); ok {
			// Send rejection without a reason
			rt.resumeChan <- ResumeReject("")
		}
		if resp, ok := ev.(*ToolCallResponseEvent); ok {
			toolResponse = resp
		}
	}

	require.NotNil(t, toolResponse, "expected a tool response event")
	require.True(t, toolResponse.Result.IsError, "expected tool result to be an error")
	require.Equal(t, "The user rejected the tool call.", toolResponse.Response)
	require.NotContains(t, toolResponse.Response, "Reason:")
}

func TestTransferTaskRejectsNonSubAgent(t *testing.T) {
	// root has librarian as sub-agent but NOT planner.
	// planner exists in the team. transfer_task to planner should be rejected.
	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}

	librarian := agent.New("librarian", "Library agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	planner := agent.New("planner", "Planner agent", agent.WithModel(prov))

	agent.WithSubAgents(librarian)(root)

	tm := team.New(team.WithAgents(root, planner, librarian))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Test"))
	evts := make(chan Event, 128)

	toolCall := tools.ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      "transfer_task",
			Arguments: `{"agent":"planner","task":"do something","expected_output":""}`,
		},
	}

	result, err := rt.handleTaskTransfer(t.Context(), sess, toolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError, "transfer to non-sub-agent should return an error result")
	assert.Contains(t, result.Output, "cannot transfer task to planner")
	assert.Contains(t, result.Output, "librarian")
	assert.Equal(t, "root", rt.currentAgent, "current agent should remain root")
}

func TestTransferTaskAllowsSubAgent(t *testing.T) {
	// Verify that transfer_task to a valid sub-agent is NOT rejected by the validation.
	// We can't fully run the child session without a real model, so we just confirm
	// it gets past validation (it will fail later due to mock stream being empty,
	// which is fine — we only care that it's not blocked by the sub-agent check).
	prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("done").AddStopWithUsage(10, 5).Build()}

	librarian := agent.New("librarian", "Library agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))

	agent.WithSubAgents(librarian)(root)

	tm := team.New(team.WithAgents(root, librarian))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
	evts := make(chan Event, 128)

	toolCall := tools.ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      "transfer_task",
			Arguments: `{"agent":"librarian","task":"find a book","expected_output":"book title"}`,
		},
	}

	result, err := rt.handleTaskTransfer(t.Context(), sess, toolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError, "transfer to valid sub-agent should succeed")
}

func TestYoloMode_OverridesPermissionsDeny(t *testing.T) {
	// Test that --yolo flag takes precedence over deny permissions
	permChecker := permissions.NewChecker(&latest.PermissionsConfig{
		Deny: []string{"dangerous_tool"},
	})

	var executed bool
	agentTools := []tools.Tool{{
		Name:       "dangerous_tool",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolCallResult, error) {
			executed = true
			return tools.ResultSuccess("executed"), nil
		},
	}}

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "You are a test agent",
		agent.WithModel(prov),
		agent.WithToolSets(newStubToolSet(nil, agentTools, nil)),
	)
	tm := team.New(
		team.WithAgents(root),
		team.WithPermissions(permChecker),
	)

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
	require.True(t, sess.ToolsApproved)

	calls := []tools.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "dangerous_tool", Arguments: "{}"},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, agentTools, events)
	close(events)

	// With --yolo, the tool should execute despite deny permission
	require.True(t, executed, "expected tool to be executed in --yolo mode despite deny permission")
}

func TestYoloMode_OverridesForceAsk(t *testing.T) {
	// Test that --yolo flag takes precedence over ForceAsk permissions
	permChecker := permissions.NewChecker(&latest.PermissionsConfig{
		Ask: []string{"careful_tool"},
	})

	var executed bool
	agentTools := []tools.Tool{{
		Name:       "careful_tool",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolCallResult, error) {
			executed = true
			return tools.ResultSuccess("executed"), nil
		},
	}}

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "You are a test agent",
		agent.WithModel(prov),
		agent.WithToolSets(newStubToolSet(nil, agentTools, nil)),
	)
	tm := team.New(
		team.WithAgents(root),
		team.WithPermissions(permChecker),
	)

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
	require.True(t, sess.ToolsApproved)

	calls := []tools.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "careful_tool", Arguments: "{}"},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, agentTools, events)
	close(events)

	// With --yolo, the tool should execute without asking
	require.True(t, executed, "expected tool to be executed in --yolo mode despite ForceAsk permission")
}

func TestYoloMode_OverridesSessionDeny(t *testing.T) {
	// Test that --yolo flag takes precedence over session-level deny
	var executed bool
	agentTools := []tools.Tool{{
		Name:       "blocked_tool",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolCallResult, error) {
			executed = true
			return tools.ResultSuccess("executed"), nil
		},
	}}

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "You are a test agent",
		agent.WithModel(prov),
		agent.WithToolSets(newStubToolSet(nil, agentTools, nil)),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(
		session.WithUserMessage("Test"),
		session.WithToolsApproved(true),
		session.WithPermissions(&session.PermissionsConfig{
			Deny: []string{"blocked_tool"},
		}),
	)
	require.True(t, sess.ToolsApproved)

	calls := []tools.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "blocked_tool", Arguments: "{}"},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, agentTools, events)
	close(events)

	// With --yolo, the tool should execute despite session deny
	require.True(t, executed, "expected tool to be executed in --yolo mode despite session deny permission")
}

func TestStripImageContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		messages []chat.Message
		want     []chat.Message
	}{
		{
			name: "no multi content unchanged",
			messages: []chat.Message{
				{Role: chat.MessageRoleUser, Content: "hello"},
				{Role: chat.MessageRoleTool, Content: "result"},
			},
			want: []chat.Message{
				{Role: chat.MessageRoleUser, Content: "hello"},
				{Role: chat.MessageRoleTool, Content: "result"},
			},
		},
		{
			name: "strips image URL parts from tool result",
			messages: []chat.Message{
				{
					Role:    chat.MessageRoleTool,
					Content: "Read image file",
					MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeText, Text: "Read image file"},
						{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "data:image/png;base64,abc"}},
					},
				},
			},
			want: []chat.Message{
				{
					Role:    chat.MessageRoleTool,
					Content: "Read image file",
					MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeText, Text: "Read image file"},
					},
				},
			},
		},
		{
			name: "strips image file parts from user message",
			messages: []chat.Message{
				{
					Role: chat.MessageRoleUser,
					MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeText, Text: "check this image"},
						{Type: chat.MessagePartTypeFile, File: &chat.MessageFile{Path: "/tmp/photo.png", MimeType: "image/png"}},
					},
				},
			},
			want: []chat.Message{
				{
					Role: chat.MessageRoleUser,
					MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeText, Text: "check this image"},
					},
				},
			},
		},
		{
			name: "preserves non-image file parts",
			messages: []chat.Message{
				{
					Role: chat.MessageRoleUser,
					MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeText, Text: "check this"},
						{Type: chat.MessagePartTypeFile, File: &chat.MessageFile{Path: "/tmp/doc.pdf", MimeType: "application/pdf"}},
					},
				},
			},
			want: []chat.Message{
				{
					Role: chat.MessageRoleUser,
					MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeText, Text: "check this"},
						{Type: chat.MessagePartTypeFile, File: &chat.MessageFile{Path: "/tmp/doc.pdf", MimeType: "application/pdf"}},
					},
				},
			},
		},
		{
			name: "mixed messages only strips images",
			messages: []chat.Message{
				{Role: chat.MessageRoleUser, Content: "plain text"},
				{
					Role: chat.MessageRoleTool,
					MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeText, Text: "tool output"},
						{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "data:image/jpeg;base64,xyz"}},
					},
				},
				{Role: chat.MessageRoleAssistant, Content: "got it"},
			},
			want: []chat.Message{
				{Role: chat.MessageRoleUser, Content: "plain text"},
				{
					Role: chat.MessageRoleTool,
					MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeText, Text: "tool output"},
					},
				},
				{Role: chat.MessageRoleAssistant, Content: "got it"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := stripImageContent(tt.messages)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestEstimateMessageTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		msg      chat.Message
		expected int64
	}{
		{
			name:     "empty message returns overhead only",
			msg:      chat.Message{},
			expected: 5, // perMessageOverhead
		},
		{
			name:     "text-only message",
			msg:      chat.Message{Content: "Hello, world!"}, // 13 chars → 13/4 = 3 + 5 overhead = 8
			expected: 8,
		},
		{
			name: "multi-content text parts",
			msg: chat.Message{
				MultiContent: []chat.MessagePart{
					{Type: chat.MessagePartTypeText, Text: "first part"},  // 10 chars
					{Type: chat.MessagePartTypeText, Text: "second part"}, // 11 chars
				},
			},
			// 21 total chars → 21/4 = 5 + 5 overhead = 10
			expected: 10,
		},
		{
			name: "message with tool calls",
			msg: chat.Message{
				ToolCalls: []tools.ToolCall{
					{
						Function: tools.FunctionCall{
							Name:      "read_file",                // 9 chars
							Arguments: `{"path":"/tmp/test.txt"}`, // 24 chars
						},
					},
				},
			},
			// 33 chars → 33/4 = 8 + 5 overhead = 13
			expected: 13,
		},
		{
			name: "message with reasoning content",
			msg: chat.Message{
				Content:          "answer",                                         // 6 chars
				ReasoningContent: "Let me think about this carefully step by step", // 47 chars
			},
			// 53 chars → 53/4 = 13 + 5 overhead = 18
			expected: 18,
		},
		{
			name: "combined content types",
			msg: chat.Message{
				Content:          "result",                                   // 6 chars
				ReasoningContent: "thinking",                                 // 8 chars
				MultiContent:     []chat.MessagePart{{Text: "extra detail"}}, // 12 chars
				ToolCalls: []tools.ToolCall{
					{Function: tools.FunctionCall{Name: "cmd", Arguments: `{"x":"y"}`}}, // 3 + 9 = 12 chars
				},
			},
			// 38 chars → 38/4 = 9 + 5 overhead = 14
			expected: 14,
		},
		{
			name: "large tool result",
			msg: chat.Message{
				Content: string(make([]byte, 40000)), // 40000 null bytes
			},
			// 40000/4 = 10000 + 5 overhead = 10005
			expected: 10005,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := estimateMessageTokens(&tt.msg)
			assert.Equal(t, tt.expected, got, "estimateMessageTokens mismatch")
		})
	}
}
