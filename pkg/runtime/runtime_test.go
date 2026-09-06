package runtime

import (
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	"github.com/docker/docker-agent/pkg/tools/codemode"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
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

// blockingToolSet models a toolset whose Tools() never returns on its own —
// e.g. a wedged MCP stdio subprocess that does not even answer context
// cancellation. It deliberately ignores ctx and blocks until release is
// closed, which tests do on cleanup so the orphaned listing goroutine exits.
type blockingToolSet struct {
	release <-chan struct{}
}

var _ tools.ToolSet = (*blockingToolSet)(nil)

func (b *blockingToolSet) Tools(context.Context) ([]tools.Tool, error) {
	<-b.release
	return nil, nil
}

// blockingStartToolSet models a toolset whose Start() never returns on its
// own — e.g. an MCP stdio server whose container never comes up because the
// Docker daemon is wedged. It deliberately ignores ctx and blocks until
// release is closed, which tests do on cleanup so the orphaned start
// goroutine exits.
type blockingStartToolSet struct {
	release <-chan struct{}
}

var (
	_ tools.ToolSet   = (*blockingStartToolSet)(nil)
	_ tools.Startable = (*blockingStartToolSet)(nil)
)

func (b *blockingStartToolSet) Start(context.Context) error {
	<-b.release
	return nil
}
func (b *blockingStartToolSet) Stop(context.Context) error                  { return nil }
func (b *blockingStartToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

// inFlightStartToolSet is a blockingStartToolSet variant that signals when a
// Start attempt is in flight (entered closes on the first call) and counts
// underlying Start calls, so tests can wedge an attempt before exercising
// the code under test.
type inFlightStartToolSet struct {
	entered   chan struct{}
	release   <-chan struct{}
	enterOnce sync.Once
	starts    atomic.Int32
}

var (
	_ tools.ToolSet   = (*inFlightStartToolSet)(nil)
	_ tools.Startable = (*inFlightStartToolSet)(nil)
)

func (b *inFlightStartToolSet) Start(context.Context) error {
	b.starts.Add(1)
	b.enterOnce.Do(func() { close(b.entered) })
	<-b.release
	return nil
}
func (b *inFlightStartToolSet) Stop(context.Context) error                  { return nil }
func (b *inFlightStartToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

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

func (b *streamBuilder) AddToolCallStopWithUsage(input, output int64) *streamBuilder {
	b.responses = append(b.responses, chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{
			Index:        0,
			FinishReason: chat.FinishReasonToolCalls,
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

func (m *mockProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(m.id) }

func (m *mockProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return m.stream, nil
}

func (m *mockProvider) BaseConfig() base.Config { return base.Config{} }

func (m *mockProvider) MaxTokens() int { return 0 }

type activeRootBlockingProvider struct {
	id      string
	release <-chan struct{}
}

func (p *activeRootBlockingProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(p.id) }

func (p *activeRootBlockingProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	select {
	case <-p.release:
		return newStreamBuilder().AddStopWithUsage(1, 1).Build(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *activeRootBlockingProvider) BaseConfig() base.Config { return base.Config{} }
func (p *activeRootBlockingProvider) MaxTokens() int          { return 0 }

type mockProviderWithError struct {
	id string
}

func (m *mockProviderWithError) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(m.id) }

func (m *mockProviderWithError) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, errors.New("simulated error creating chat completion stream")
}

func (m *mockProviderWithError) BaseConfig() base.Config { return base.Config{} }

func (m *mockProviderWithError) MaxTokens() int { return 0 }

type mockModelStore struct {
	ModelStore
}

func (m mockModelStore) GetModel(_ context.Context, _ modelsdev.ID) (*modelsdev.Model, error) {
	return nil, nil
}

func runSession(t *testing.T, sess *session.Session, stream *mockStream) []Event {
	t.Helper()

	prov := &mockProvider{id: "test/mock-model", stream: stream}
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
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
	t.Parallel()

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
		TeamInfo([]AgentDetails{{Name: "root", Provider: "test", Model: "mock-model"}}, "root"),
		ToolsetInfo(0, false, "root"),
		UserMessage("Hi", sess.ID, nil, 0),
		StreamStarted(sess.ID, "root"),
		ToolsetInfo(0, false, "root"),
		AgentInfo("root", "test/mock-model", "", ""),
		AgentChoice("root", sess.ID, "Hello"),
		MessageAdded(sess.ID, msgAdded.Message, "root"),
		NewTokenUsageEvent(sess.ID, "root", &Usage{InputTokens: 3, OutputTokens: 2, ContextLength: 5, LastMessage: &MessageUsage{
			Usage:        chat.Usage{InputTokens: 3, OutputTokens: 2},
			Model:        "test/mock-model",
			FinishReason: chat.FinishReasonStop,
		}}),
		StreamStopped(sess.ID, "root", "normal"),
	}

	assertEventsEqual(t, expectedEvents, events)
}

func TestMultipleContentChunks(t *testing.T) {
	t.Parallel()

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
		TeamInfo([]AgentDetails{{Name: "root", Provider: "test", Model: "mock-model"}}, "root"),
		ToolsetInfo(0, false, "root"),
		UserMessage("Please greet me", sess.ID, nil, 0),
		StreamStarted(sess.ID, "root"),
		ToolsetInfo(0, false, "root"),
		AgentInfo("root", "test/mock-model", "", ""),
		AgentChoice("root", sess.ID, "Hello "),
		AgentChoice("root", sess.ID, "there, "),
		AgentChoice("root", sess.ID, "how "),
		AgentChoice("root", sess.ID, "are "),
		AgentChoice("root", sess.ID, "you?"),
		MessageAdded(sess.ID, msgAdded.Message, "root"),
		NewTokenUsageEvent(sess.ID, "root", &Usage{InputTokens: 8, OutputTokens: 12, ContextLength: 20, LastMessage: &MessageUsage{
			Usage:        chat.Usage{InputTokens: 8, OutputTokens: 12},
			Model:        "test/mock-model",
			FinishReason: chat.FinishReasonStop,
		}}),
		StreamStopped(sess.ID, "root", "normal"),
	}

	assertEventsEqual(t, expectedEvents, events)
}

func TestWithReasoning(t *testing.T) {
	t.Parallel()

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
		TeamInfo([]AgentDetails{{Name: "root", Provider: "test", Model: "mock-model"}}, "root"),
		ToolsetInfo(0, false, "root"),
		UserMessage("Hi", sess.ID, nil, 0),
		StreamStarted(sess.ID, "root"),
		ToolsetInfo(0, false, "root"),
		AgentInfo("root", "test/mock-model", "", ""),
		AgentChoiceReasoning("root", sess.ID, "Let me think about this..."),
		AgentChoiceReasoning("root", sess.ID, " I should respond politely."),
		AgentChoice("root", sess.ID, "Hello, how can I help you?"),
		MessageAdded(sess.ID, msgAdded.Message, "root"),
		NewTokenUsageEvent(sess.ID, "root", &Usage{InputTokens: 10, OutputTokens: 15, ContextLength: 25, LastMessage: &MessageUsage{
			Usage:        chat.Usage{InputTokens: 10, OutputTokens: 15},
			Model:        "test/mock-model",
			FinishReason: chat.FinishReasonStop,
		}}),
		StreamStopped(sess.ID, "root", "normal"),
	}

	assertEventsEqual(t, expectedEvents, events)
}

func TestMixedContentAndReasoning(t *testing.T) {
	t.Parallel()

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
		TeamInfo([]AgentDetails{{Name: "root", Provider: "test", Model: "mock-model"}}, "root"),
		ToolsetInfo(0, false, "root"),
		UserMessage("Hi there", sess.ID, nil, 0),
		StreamStarted(sess.ID, "root"),
		ToolsetInfo(0, false, "root"),
		AgentInfo("root", "test/mock-model", "", ""),
		AgentChoiceReasoning("root", sess.ID, "The user wants a greeting"),
		AgentChoice("root", sess.ID, "Hello!"),
		AgentChoiceReasoning("root", sess.ID, " I should be friendly"),
		AgentChoice("root", sess.ID, " How can I help you today?"),
		MessageAdded(sess.ID, msgAdded.Message, "root"),
		NewTokenUsageEvent(sess.ID, "root", &Usage{InputTokens: 15, OutputTokens: 20, ContextLength: 35, LastMessage: &MessageUsage{
			Usage:        chat.Usage{InputTokens: 15, OutputTokens: 20},
			Model:        "test/mock-model",
			FinishReason: chat.FinishReasonStop,
		}}),
		StreamStopped(sess.ID, "root", "normal"),
	}

	assertEventsEqual(t, expectedEvents, events)
}

func TestToolCallSequence(t *testing.T) {
	t.Parallel()

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

func TestRunStreamIncrementsActiveRootStreamsBeforeReturning(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	prov := &activeRootBlockingProvider{id: "test/mock-model", release: release}
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	events := rt.RunStream(ctx, session.New(session.WithUserMessage("block")))
	assert.Equal(t, int32(1), rt.activeRootStreams.Load())

	cancel()
	for range events {
	}
	assert.Equal(t, int32(0), rt.activeRootStreams.Load())
	close(release)
}

func TestRecallUsesSteerWhileRootStreamActive(t *testing.T) {
	t.Parallel()

	rt := &LocalRuntime{steerQueue: NewInMemoryMessageQueue(1)}
	rt.activeRootStreams.Store(1)
	handlerCalled := false
	rt.SetRecallHandler(func(context.Context, QueuedMessage) bool {
		handlerCalled = true
		return true
	})

	recall := QueuedMessage{Content: "job finished"}
	require.NoError(t, rt.recall(t.Context(), recall))
	assert.False(t, handlerCalled)

	got, ok := rt.steerQueue.Dequeue(t.Context())
	require.True(t, ok)
	assert.Equal(t, recall, got)
}

func TestProcessToolCallsRecallEnqueuesSteeringMessage(t *testing.T) {
	t.Parallel()

	const recallMessage = "Background job job_1 finished with status completed.\n--- Output ---\nhello"
	agentTools := []tools.Tool{{
		Name:       "recall_tool",
		Parameters: map[string]any{},
		Handler: func(ctx context.Context, _ tools.ToolCall, rt tools.Runtime) (*tools.ToolCallResult, error) {
			require.NoError(t, rt.Recall(ctx, recallMessage))
			return tools.ResultSuccess("started"), nil
		},
	}}

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "You are a test agent",
		agent.WithModel(prov),
		agent.WithToolSets(newStubToolSet(nil, agentTools, nil)),
	)
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
	events := make(chan Event, 16)
	stopRun, stopMsg := rt.processToolCalls(t.Context(), sess, []tools.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "recall_tool", Arguments: "{}"},
	}}, agentTools, NewChannelSink(events))
	require.False(t, stopRun)
	require.Empty(t, stopMsg)

	sr := rt.drainAndEmitSteered(t.Context(), sess, root, NewChannelSink(events))
	require.True(t, sr.drained)
	close(events)

	var sawRecall bool
	for ev := range events {
		if msg, ok := ev.(*UserMessageEvent); ok && msg.Message == recallMessage {
			sawRecall = true
		}
	}
	assert.True(t, sawRecall, "recall message should be emitted as a steered user message")
}

// TestXMLToolCallFallback verifies that <tool_call> blocks in text content
// are extracted as tool calls and not leaked as AgentChoice events.
func TestXMLToolCallFallback(t *testing.T) {
	t.Parallel()

	xmlPayload := `<tool_call>
{"name": "shell_exec", "arguments": {"cmd": "ls -la"}}
</tool_call>`

	stream := newStreamBuilder().
		AddContent(xmlPayload).
		AddStopWithUsage(10, 15).
		Build()

	sess := session.New(session.WithUserMessage("list files"))
	events := runSession(t, sess, stream)

	// The XML should be promoted to a PartialToolCall, not remain as plain text.
	require.True(t, hasEventType(t, events, &PartialToolCallEvent{}), "Expected PartialToolCallEvent from XML extraction")

	// No raw XML should have been emitted as an AgentChoice event.
	for _, ev := range events {
		if choice, ok := ev.(*AgentChoiceEvent); ok {
			require.NotContains(t, choice.Content, "<tool_call>", "XML tool call block must not appear in AgentChoice events")
		}
	}

	// Verify the extracted tool call fields.
	for _, ev := range events {
		if partial, ok := ev.(*PartialToolCallEvent); ok {
			require.Equal(t, "shell_exec", partial.ToolCall.Function.Name)
			require.JSONEq(t, `{"cmd": "ls -la"}`, partial.ToolCall.Function.Arguments)
		}
	}
}

// TestXMLToolCallFallback_WithPreamble verifies that preamble text before a
// <tool_call> block is emitted as AgentChoice while the XML is suppressed.
func TestXMLToolCallFallback_WithPreamble(t *testing.T) {
	t.Parallel()

	stream := newStreamBuilder().
		AddContent("I'll list the files for you.\n").
		AddContent(`<tool_call>{"name": "ls", "arguments": {"path": "/tmp"}}</tool_call>`).
		AddStopWithUsage(12, 18).
		Build()

	sess := session.New(session.WithUserMessage("list /tmp"))
	events := runSession(t, sess, stream)

	// Preamble text must have been emitted as AgentChoice.
	var choiceContents []string
	for _, ev := range events {
		if choice, ok := ev.(*AgentChoiceEvent); ok {
			choiceContents = append(choiceContents, choice.Content)
		}
	}
	require.NotEmpty(t, choiceContents, "Expected AgentChoice events for preamble")
	require.Contains(t, strings.Join(choiceContents, ""), "I'll list the files for you.")

	// XML itself must not leak into AgentChoice.
	for _, c := range choiceContents {
		require.NotContains(t, c, "<tool_call>")
	}

	// Tool must still be extracted.
	require.True(t, hasEventType(t, events, &PartialToolCallEvent{}))
}

func TestErrorEvent(t *testing.T) {
	t.Parallel()

	prov := &mockProviderWithError{id: "test/error-model"}
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Hi"))
	sess.Title = "Unit Test"

	evCh := rt.RunStream(t.Context(), sess)

	var events []Event
	for ev := range evCh {
		events = append(events, ev)
	}

	require.Len(t, events, 8)
	require.IsType(t, &TeamInfoEvent{}, events[0])
	require.IsType(t, &ToolsetInfoEvent{}, events[1])
	require.IsType(t, &UserMessageEvent{}, events[2])
	require.IsType(t, &StreamStartedEvent{}, events[3])
	require.IsType(t, &ToolsetInfoEvent{}, events[4])
	require.IsType(t, &AgentInfoEvent{}, events[5])
	require.IsType(t, &ErrorEvent{}, events[6])
	require.IsType(t, &StreamStoppedEvent{}, events[7])

	errorEvent := events[6].(*ErrorEvent)
	require.Contains(t, errorEvent.Error, "simulated error")
}

func TestContextCancellation(t *testing.T) {
	t.Parallel()

	stream := newStreamBuilder().
		AddContent("This should not complete").
		AddStopWithUsage(10, 5).
		Build()

	prov := &mockProvider{id: "test/mock-model", stream: stream}
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
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

	require.GreaterOrEqual(t, len(events), 4)
	require.IsType(t, &TeamInfoEvent{}, events[0])
	require.IsType(t, &ToolsetInfoEvent{}, events[1])
	require.IsType(t, &UserMessageEvent{}, events[2])
	require.IsType(t, &StreamStartedEvent{}, events[3])
	require.IsType(t, &StreamStoppedEvent{}, events[len(events)-1])
}

func TestToolCallVariations(t *testing.T) {
	t.Parallel()

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

func (p *queueProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(p.id) }

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

func (m mockModelStoreWithLimit) GetModel(_ context.Context, _ modelsdev.ID) (*modelsdev.Model, error) {
	return &modelsdev.Model{Limit: modelsdev.Limit{Context: m.limit}, Cost: &modelsdev.Cost{}}, nil
}

func TestCompaction(t *testing.T) {
	t.Parallel()

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
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(true), WithModelStore(mockModelStoreWithLimit{limit: 100}))
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

// errorProvider always returns the configured error from CreateChatCompletionStream.
type errorProvider struct {
	id  string
	err error
}

func (p *errorProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(p.id) }

func (p *errorProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, p.err
}

func (p *errorProvider) BaseConfig() base.Config { return base.Config{} }

func (p *errorProvider) MaxTokens() int { return 0 }

func TestCompactionOverflowDoesNotLoop(t *testing.T) {
	t.Parallel()

	// The model always returns a ContextOverflowError. Without the
	// max-retry guard this would loop forever because compaction
	// cannot fix the problem.
	overflowErr := modelerrors.NewContextOverflowError(errors.New("prompt is too long"))
	prov := &errorProvider{id: "test/overflow-model", err: overflowErr}

	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(true), WithModelStore(mockModelStoreWithLimit{limit: 100}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Hello"))
	events := rt.RunStream(t.Context(), sess)

	var compactionCount int
	var sawError bool
	for ev := range events {
		if e, ok := ev.(*SessionCompactionEvent); ok && e.Status == "started" {
			compactionCount++
		}
		if _, ok := ev.(*ErrorEvent); ok {
			sawError = true
		}
	}

	// Compaction should have been attempted at most once, then the loop
	// must give up and surface an error instead of retrying indefinitely.
	require.LessOrEqual(t, compactionCount, 1, "expected at most 1 compaction attempt, got %d", compactionCount)
	require.True(t, sawError, "expected an ErrorEvent after exhausting compaction retries")
}

func TestSessionWithoutUserMessage(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
			rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(mockModelStore{}))
			require.NoError(t, err)

			events := make(chan Event, 10)
			sessionSpan := trace.SpanFromContext(t.Context())

			// First call
			tools1, err := rt.getTools(t.Context(), session.New(), root, sessionSpan, NewChannelSink(events), true)
			require.NoError(t, err)
			require.Len(t, tools1, tt.wantToolCount)

			rt.emitAgentWarnings(root, NewChannelSink(events))
			evs := collectEvents(events)
			require.Equal(t, tt.wantWarning, hasWarningEvent(evs), "warning event mismatch on first call")
		})
	}
}

func TestNewRuntime_NoAgentsError(t *testing.T) {
	t.Parallel()

	tm := team.New()

	_, err := New(t.Context(), tm, WithModelStore(mockModelStore{}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "no agents loaded")
}

func TestNewRuntime_InvalidCurrentAgentError(t *testing.T) {
	t.Parallel()

	root := agent.New("root", "You are a test agent")
	tm := team.New(team.WithAgents(root))

	// Ask for a non-existent current agent
	_, err := New(t.Context(), tm, WithCurrentAgent("other"), WithModelStore(mockModelStore{}))
	require.Contains(t, err.Error(), "agent not found: other (available agents: root)")
}

// TestNewRuntime_ModelStorePrecedence pins the resolution order for the
// runtime's models.dev store: an explicit WithModelStore wins; otherwise a
// store carried on the ModelSwitcherConfig is adopted (this is how the team
// loader shares the catalog it already warmed); otherwise the runtime builds
// its own lazy store. Sharing the warmed store is what keeps the first /model
// open from re-paying the multi-MB catalog parse.
func TestNewRuntime_ModelStorePrecedence(t *testing.T) {
	t.Parallel()

	newTeam := func() *team.Team {
		root := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/m"}))
		return team.New(team.WithAgents(root))
	}

	t.Run("explicit WithModelStore wins over ModelSwitcherConfig", func(t *testing.T) {
		t.Parallel()
		explicit := &mockCatalogStore{}
		cfgStore := &mockCatalogStore{}
		rt, err := NewLocalRuntime(t.Context(), newTeam(),
			WithModelStore(explicit),
			WithModelSwitcherConfig(&ModelSwitcherConfig{ModelsStore: cfgStore}),
		)
		require.NoError(t, err)
		assert.Same(t, explicit, rt.modelsStore)
	})

	t.Run("ModelSwitcherConfig store is adopted when no explicit store", func(t *testing.T) {
		t.Parallel()
		cfgStore := &mockCatalogStore{}
		rt, err := NewLocalRuntime(t.Context(), newTeam(),
			WithModelSwitcherConfig(&ModelSwitcherConfig{ModelsStore: cfgStore}),
		)
		require.NoError(t, err)
		assert.Same(t, cfgStore, rt.modelsStore)
	})

	t.Run("falls back to lazy store when neither is set", func(t *testing.T) {
		t.Parallel()
		rt, err := NewLocalRuntime(t.Context(), newTeam(),
			WithModelSwitcherConfig(&ModelSwitcherConfig{}),
		)
		require.NoError(t, err)
		assert.IsType(t, &lazyModelStore{}, rt.modelsStore)
	})
}

func TestProcessToolCalls_UnknownTool_ReturnsErrorResponse(t *testing.T) {
	t.Parallel()

	root := agent.New("root", "You are a test agent", agent.WithModel(&mockProvider{}))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	rt.registerDefaultTools()

	sess := session.New(session.WithUserMessage("Start"))

	calls := []tools.ToolCall{{
		ID:       "tool-unknown-1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "non_existent_tool", Arguments: "{}"},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, nil, NewChannelSink(events))
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

// oauthAwareToolSet simulates a remote MCP toolset that needs an elicitation
// handler and the managed-OAuth flag configured before Start() runs. The
// Slack-MCP bug reported by users shows up exactly when Start() triggers an
// OAuth flow with neither handler installed, so this test captures the
// handler state at the moment Start() is entered.
type oauthAwareToolSet struct {
	mu                   sync.Mutex
	elicitationHandler   tools.ElicitationHandler
	managedOAuth         bool
	managedOAuthSet      bool
	started              bool
	startHandlerCaptured tools.ElicitationHandler
	startManagedCaptured bool
	startManagedWasSet   bool
}

// Verify interface compliance
var (
	_ tools.ToolSet      = (*oauthAwareToolSet)(nil)
	_ tools.Startable    = (*oauthAwareToolSet)(nil)
	_ tools.Elicitable   = (*oauthAwareToolSet)(nil)
	_ tools.OAuthCapable = (*oauthAwareToolSet)(nil)
)

func (s *oauthAwareToolSet) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = true
	// Snapshot the handler state at the moment Start runs — this is what
	// the OAuth flow would see when it tries to prompt the user.
	s.startHandlerCaptured = s.elicitationHandler
	s.startManagedCaptured = s.managedOAuth
	s.startManagedWasSet = s.managedOAuthSet
	return nil
}

func (s *oauthAwareToolSet) Stop(context.Context) error { return nil }

func (s *oauthAwareToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return nil, nil
}

func (s *oauthAwareToolSet) SetElicitationHandler(h tools.ElicitationHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.elicitationHandler = h
}

func (s *oauthAwareToolSet) SetOAuthSuccessHandler(func()) {}

func (s *oauthAwareToolSet) SetManagedOAuth(managed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.managedOAuth = managed
	s.managedOAuthSet = true
}

func (s *oauthAwareToolSet) SetUnmanagedOAuthRedirectURI(string) {}

// TestEmitStartupInfo_DoesNotBlockOnInteractiveOAuth verifies that the
// startup path does NOT trigger interactive flows on toolsets. In particular:
//
//   - EmitStartupInfo must complete promptly even when a toolset's Start()
//     would normally prompt the user (e.g. an OAuth elicitation for a remote
//     MCP server).
//   - The runtime's elicitation/OAuth handlers must not be wired into the
//     toolset during startup; the OAuth dialog only makes sense once the
//     user is interacting with the agent.
//
// Regression test for: "docker agent run ./examples/slack.yaml" hanging
// before the TUI was even ready, with Ctrl-C unable to interrupt because
// the OAuth elicitation was synchronously blocked on a TUI dialog that the
// app hadn't started yet. The fix marks the startup context with
// mcptools.WithoutInteractivePrompts and defers OAuth to the first
// RunStream call.
func TestEmitStartupInfo_DoesNotBlockOnInteractiveOAuth(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}

	oauthTS := &oauthAwareToolSet{}

	root := agent.New("root", "agent",
		agent.WithModel(prov),
		agent.WithToolSets(oauthTS),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("root"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	events := make(chan Event, 20)

	done := make(chan struct{})
	go func() {
		rt.EmitStartupInfo(t.Context(), nil, NewChannelSink(events))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("EmitStartupInfo blocked: it must complete promptly even for toolsets that need OAuth")
	}
	close(events)
	for range events {
	}

	oauthTS.mu.Lock()
	defer oauthTS.mu.Unlock()

	require.True(t, oauthTS.started, "toolset should still be started during EmitStartupInfo (just not interactively)")

	// During startup, no interactive plumbing should be wired up. OAuth and
	// elicitation are deferred to the first RunStream call where the user
	// is actively interacting with the agent.
	require.Nil(t, oauthTS.startHandlerCaptured,
		"elicitation handler must NOT be set during startup; OAuth is deferred until the user sends a message")
	require.False(t, oauthTS.startManagedWasSet,
		"managed-OAuth flag must NOT be set during startup")
}

// TestEmitStartupInfo_SurfacesToolsetStartFailureAsWarning verifies that
// when a toolset fails to start during EmitStartupInfo, the failure is
// emitted as a WarningEvent on the events channel so the TUI can show
// the user the actual cause — not just silently drop the toolset.
//
// Without this, a remote MCP server returning a 4xx during initialize
// (e.g. Slack's "App is not enabled for Slack MCP server access")
// disappears from the sidebar with only a debug-log trace, leaving the
// user with no hint about what went wrong.
func TestEmitStartupInfo_SurfacesToolsetStartFailureAsWarning(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}

	// A toolset whose Start() always fails with a rich, provider-specific
	// message — mimicking the error returned by remoteMCPClient.Initialize
	// after it has been enriched with the server's own explanation.
	failingTS := newStubToolSet(
		errors.New("failed to initialize MCP client: failed to connect to MCP server: sending \"initialize\": Bad Request (server responded 400: App is not enabled for Slack MCP server access.)"),
		nil,
		nil,
	)

	root := agent.New("root", "agent",
		agent.WithModel(prov),
		agent.WithToolSets(failingTS),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("root"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	events := make(chan Event, 32)
	rt.EmitStartupInfo(t.Context(), nil, NewChannelSink(events))
	close(events)

	var warning *WarningEvent
	for e := range events {
		if w, ok := e.(*WarningEvent); ok {
			warning = w
		}
	}

	require.NotNil(t, warning, "EmitStartupInfo should emit a WarningEvent when a toolset fails to start")
	assert.Contains(t, warning.Message, "App is not enabled for Slack MCP server access.",
		"warning should include the toolset's actual error message so the user can see the real cause")
}

// TestEmitStartupInfo_SkipsToolsetWhoseListingHangs is the regression test for
// issue #3137: a toolset whose Tools() blocks indefinitely must not stall the
// whole startup tool-loading loop. Before the per-toolset timeout,
// emitToolsProgressively blocked forever on the hung toolset, so the terminal
// ToolsetInfo{Loading:false} was never emitted (sidebar stuck on "Loading
// tools…") and EmitStartupInfo's goroutine leaked, also delaying /quit.
func TestEmitStartupInfo_SkipsToolsetWhoseListingHangs(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}

	// release is closed on cleanup so the orphaned listing goroutine (whose
	// Tools() ignores context cancellation) exits instead of leaking.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	hanging := &blockingToolSet{release: release}
	fast := newStubToolSet(nil, []tools.Tool{{Name: "ready"}}, nil)

	root := agent.New("root", "agent",
		agent.WithModel(prov),
		agent.WithToolSets(hanging, fast),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm,
		WithCurrentAgent("root"),
		WithModelStore(mockModelStore{}),
		WithToolListTimeout(50*time.Millisecond),
	)
	require.NoError(t, err)

	events := make(chan Event, 32)
	done := make(chan struct{})
	go func() {
		rt.EmitStartupInfo(t.Context(), nil, NewChannelSink(events))
		close(events)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("EmitStartupInfo did not return: a hung toolset blocked startup tool loading")
	}

	var toolsetInfos []*ToolsetInfoEvent
	for e := range events {
		if ti, ok := e.(*ToolsetInfoEvent); ok {
			toolsetInfos = append(toolsetInfos, ti)
		}
	}

	require.NotEmpty(t, toolsetInfos, "expected at least one ToolsetInfo event")
	last := toolsetInfos[len(toolsetInfos)-1]
	assert.False(t, last.Loading, "final ToolsetInfo must report Loading=false so the sidebar resolves")
	assert.Equal(t, 1, last.AvailableTools,
		"the hung toolset is skipped; the fast toolset's single tool is still counted")
}

// TestEmitStartupInfo_SkipsToolsetWhoseStartHangs is the companion of the
// listing-hang test above for the Start() phase: a toolset whose Start()
// blocks indefinitely (e.g. an MCP server container that Docker never manages
// to launch) must not stall startup tool loading. Without the per-toolset
// start timeout the terminal ToolsetInfo{Loading:false} was never emitted, so
// the sidebar animated "tools available…" forever.
func TestEmitStartupInfo_SkipsToolsetWhoseStartHangs(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}

	// release is closed on cleanup so the orphaned start goroutine (whose
	// Start() ignores context cancellation) exits instead of leaking.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	hanging := &blockingStartToolSet{release: release}
	fast := newStubToolSet(nil, []tools.Tool{{Name: "ready"}}, nil)

	root := agent.New("root", "agent",
		agent.WithModel(prov),
		agent.WithToolSets(hanging, fast),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm,
		WithCurrentAgent("root"),
		WithModelStore(mockModelStore{}),
		WithToolStartTimeout(50*time.Millisecond),
	)
	require.NoError(t, err)

	events := make(chan Event, 32)
	done := make(chan struct{})
	go func() {
		rt.EmitStartupInfo(t.Context(), nil, NewChannelSink(events))
		close(events)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("EmitStartupInfo did not return: a toolset with a hung Start blocked startup tool loading")
	}

	var toolsetInfos []*ToolsetInfoEvent
	var warning *WarningEvent
	for e := range events {
		switch ev := e.(type) {
		case *ToolsetInfoEvent:
			toolsetInfos = append(toolsetInfos, ev)
		case *WarningEvent:
			warning = ev
		}
	}

	require.NotEmpty(t, toolsetInfos, "expected at least one ToolsetInfo event")
	last := toolsetInfos[len(toolsetInfos)-1]
	assert.False(t, last.Loading, "final ToolsetInfo must report Loading=false so the sidebar spinner stops")
	assert.Equal(t, 1, last.AvailableTools,
		"the hung toolset is skipped; the fast toolset's single tool is still counted")

	require.NotNil(t, warning, "a start timeout must surface a user-visible warning")
	assert.Contains(t, warning.Message, "taking too long to start")
}

// TestEmitStartupInfo_SkipsToolsetWhoseStartIsAlreadyInFlight pins the
// skip-in-flight half of the bounded-start contract (tools.TryStartWithTimeout):
// a toolset whose Start is already running — e.g. abandoned by an earlier
// bounded attempt and still holding the single-flight lock — is skipped
// immediately instead of joined, so startup neither burns the start budget
// again nor emits a duplicate "taking too long" warning, and the fast
// toolset is still counted.
func TestEmitStartupInfo_SkipsToolsetWhoseStartIsAlreadyInFlight(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}

	release := make(chan struct{})
	releaseWedged := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseWedged)

	inFlight := &inFlightStartToolSet{entered: make(chan struct{}), release: release}
	fast := newStubToolSet(nil, []tools.Tool{{Name: "ready"}}, nil)

	root := agent.New("root", "agent",
		agent.WithModel(prov),
		agent.WithToolSets(inFlight, fast),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm,
		WithCurrentAgent("root"),
		WithModelStore(mockModelStore{}),
		// Long enough that a regression to joining the in-flight attempt
		// trips the prompt-return guard below instead of passing slowly.
		WithToolStartTimeout(30*time.Second),
	)
	require.NoError(t, err)

	// Wedge the toolset's Start before startup runs, as an earlier abandoned
	// bounded attempt would: it keeps holding the single-flight lock.
	startable, ok := root.ToolSets()[0].(*tools.StartableToolSet)
	require.True(t, ok)
	wedgedDone := make(chan error, 1)
	go func() { wedgedDone <- startable.Start(t.Context()) }()
	<-inFlight.entered

	events := make(chan Event, 32)
	done := make(chan struct{})
	go func() {
		rt.EmitStartupInfo(t.Context(), nil, NewChannelSink(events))
		close(events)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("EmitStartupInfo did not return promptly: an in-flight toolset start was joined instead of skipped")
	}

	var toolsetInfos []*ToolsetInfoEvent
	var warnings []*WarningEvent
	for e := range events {
		switch ev := e.(type) {
		case *ToolsetInfoEvent:
			toolsetInfos = append(toolsetInfos, ev)
		case *WarningEvent:
			warnings = append(warnings, ev)
		}
	}

	require.NotEmpty(t, toolsetInfos, "expected at least one ToolsetInfo event")
	last := toolsetInfos[len(toolsetInfos)-1]
	assert.False(t, last.Loading, "final ToolsetInfo must report Loading=false so the sidebar resolves")
	assert.Equal(t, 1, last.AvailableTools,
		"the in-flight toolset is skipped; the fast toolset's single tool is still counted")
	assert.Empty(t, warnings, "a skipped in-flight start must not surface a warning")
	assert.EqualValues(t, 1, inFlight.starts.Load(), "the skip must not run a second underlying Start")

	// Unblock the wedged attempt so its goroutine exits before the test ends.
	releaseWedged()
	require.NoError(t, <-wedgedDone)
}

// TestEmitStartupInfo_ReturnsOnCancelWhileStartIsWedged is the regression
// test for the cancellation half of the bounded-start contract: when the
// EmitStartupInfo context is canceled while TryStartWithTimeout has abandoned
// a wedged Start that still holds the toolset's single-flight lock, the
// startup pass must return instead of consulting the failure reporters,
// which share that lock and would block on it forever. The bubble makes the
// race deterministic: synctest.Wait parks the emit loop on the start outcome
// — past its top-of-loop ctx check — before the cancel fires, and a
// regression to the blocking reporters deadlocks the bubble.
func TestEmitStartupInfo_ReturnsOnCancelWhileStartIsWedged(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}

		// release is closed on cleanup so the wedged start goroutine (whose
		// Start() ignores context cancellation) exits before the bubble is
		// checked for leaked goroutines.
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })

		wedged := &inFlightStartToolSet{entered: make(chan struct{}), release: release}

		root := agent.New("root", "agent",
			agent.WithModel(prov),
			agent.WithToolSets(wedged),
		)
		tm := team.New(team.WithAgents(root))

		rt, err := NewLocalRuntime(t.Context(), tm,
			WithCurrentAgent("root"),
			WithModelStore(mockModelStore{}),
			// Long enough (in the bubble's fake time) that the per-toolset
			// deadline cannot fire; only the cancel unblocks the attempt.
			WithToolStartTimeout(time.Hour),
		)
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(t.Context())
		events := make(chan Event, 32)
		done := make(chan struct{})
		go func() {
			rt.EmitStartupInfo(ctx, nil, NewChannelSink(events))
			close(done)
		}()

		// The startup pass's own bounded attempt has entered the wedged
		// Start — it now holds the single-flight lock. Park every other
		// goroutine (the emit loop is blocked on the start outcome, past its
		// top-of-loop ctx check) before canceling, so the cancellation
		// deterministically loses that check's race.
		<-wedged.entered
		synctest.Wait()
		cancel()

		// EmitStartupInfo must return while the underlying Start is still
		// wedged: release is closed only by the cleanup above, after this
		// receive. Before the fix, the emit loop hung on the failure
		// reporters' lock here and the bubble deadlocked.
		<-done

		close(events)
		for e := range events {
			if w, ok := e.(*WarningEvent); ok {
				t.Fatalf("cancellation during a wedged start must not surface a warning; got %q", w.Message)
			}
		}
		assert.EqualValues(t, 1, wedged.starts.Load(), "exactly one underlying Start ran and stayed wedged")
	})
}

// TestEmitStartupInfo_EmitsProgressWhileSlowToolsetStarts guards the
// progressive contract of emitToolsProgressively: an early fast toolset must
// be counted in a ToolsetInfo event while a later toolset is still starting,
// not only after every start has settled.
func TestEmitStartupInfo_EmitsProgressWhileSlowToolsetStarts(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}

	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	fast := newStubToolSet(nil, []tools.Tool{{Name: "ready"}}, nil)
	slow := &blockingStartToolSet{release: release}

	root := agent.New("root", "agent",
		agent.WithModel(prov),
		agent.WithToolSets(fast, slow),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm,
		WithCurrentAgent("root"),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	events := make(chan Event, 32)
	go func() {
		rt.EmitStartupInfo(t.Context(), nil, NewChannelSink(events))
		close(events)
	}()

	// The fast toolset's count must arrive while the slow start is still
	// blocked (release is not closed yet).
	timeout := time.After(5 * time.Second)
	for {
		select {
		case e := <-events:
			ti, ok := e.(*ToolsetInfoEvent)
			if !ok {
				continue
			}
			if ti.AvailableTools == 1 && ti.Loading {
				close(release)
				for range events { // drain so EmitStartupInfo finishes
				}
				return
			}
		case <-timeout:
			t.Fatal("no progress event for the fast toolset arrived while the slow toolset was starting")
		}
	}
}

// TestEmitStartupInfo_AuthRequiredIsSilent verifies that when a toolset's
// Start() returns an mcptools.IsAuthorizationRequired error — the runtime
// deliberately deferred OAuth until the user is interacting — the user
// sees no warning event for it. The OAuth dialog will appear naturally on
// the first RunStream, so a pre-announcement would just be noise.
func TestEmitStartupInfo_AuthRequiredIsSilent(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}

	deferralErr := &mcptools.AuthorizationRequiredError{URL: "https://example.test/mcp"}
	require.True(t, mcptools.IsAuthorizationRequired(deferralErr),
		"sanity: AuthorizationRequiredError must be detected by IsAuthorizationRequired")

	failingTS := newStubToolSet(deferralErr, nil, nil)

	root := agent.New("root", "agent",
		agent.WithModel(prov),
		agent.WithToolSets(failingTS),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("root"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	events := make(chan Event, 32)
	rt.EmitStartupInfo(t.Context(), nil, NewChannelSink(events))
	close(events)

	for e := range events {
		if w, ok := e.(*WarningEvent); ok {
			t.Fatalf("deferred-OAuth must not produce a WarningEvent (would be redundant noise); got: %q", w.Message)
		}
	}
}

// TestEmitStartupInfo_DeferredAuthDoesNotConsumeFailureGate verifies that
// when a toolset's Start fails with AuthorizationRequiredError during the
// non-interactive startup phase, the StartableToolSet's once-per-streak
// gate is LEFT INTACT — not silently consumed by the "is this the first
// failure?" check.
//
// Why this matters: the deferred-OAuth case is an *expected*, transient
// failure. The first user-visible failure that should produce a warning is
// whatever happens on the eventual interactive retry (e.g. "server
// responded 400: App is not enabled for Slack MCP server access"). If the
// gate is consumed during startup, the StartableToolSet's once-per-streak
// guard fires for the deferred case and silently swallows the real cause,
// leaving the user staring at "0 tools" with nothing in the UI explaining
// why.
func TestEmitStartupInfo_DeferredAuthDoesNotConsumeFailureGate(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}

	deferralErr := &mcptools.AuthorizationRequiredError{URL: "https://example.test/mcp"}
	failingTS := newStubToolSet(deferralErr, nil, nil)

	root := agent.New("root", "agent",
		agent.WithModel(prov),
		agent.WithToolSets(failingTS),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("root"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	events := make(chan Event, 32)
	rt.EmitStartupInfo(t.Context(), nil, NewChannelSink(events))
	close(events)
	for range events {
	}

	// Locate the StartableToolSet wrapping our stub so we can probe its
	// internal state (the public API uses ShouldReportFailure as both the
	// query and the consume operation).
	var wrapped *tools.StartableToolSet
	for _, ts := range root.ToolSets() {
		if s, ok := ts.(*tools.StartableToolSet); ok {
			wrapped = s
			break
		}
	}
	require.NotNil(t, wrapped, "agent.ToolSets() should return a *StartableToolSet wrapper")

	require.True(t, wrapped.ShouldReportFailure(),
		"deferred-OAuth must NOT consume the failure-reported gate during EmitStartupInfo: "+
			"otherwise the next real failure (Slack 4xx after OAuth, etc.) is silently dropped "+
			"and the user sees zero tools with no explanation")
}

// recoveryAuthToolSet simulates a toolset whose first Start() always succeeds,
// and whose Restart() returns a configurable error (used to simulate a
// background invalid_token loss after a prior successful start).
// IsStarted() reflects live connection state so StartableToolSet.Start() can
// detect the "inner went dead" recovery scenario.
type recoveryAuthToolSet struct {
	started    bool
	restartErr error
}

func (r *recoveryAuthToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (r *recoveryAuthToolSet) Start(context.Context) error                 { r.started = true; return nil }

func (r *recoveryAuthToolSet) Stop(context.Context) error    { r.started = false; return nil }
func (r *recoveryAuthToolSet) IsStarted() bool               { return r.started }
func (r *recoveryAuthToolSet) Restart(context.Context) error { return r.restartErr }

// TestEmitStartupInfo_RecoveryAuthNoticeEmittedOnce is the regression test for
// blocking issue 3: when a toolset was previously started and working but the
// background watcher detected a server-side invalid_token, the next call to
// emitToolsProgressively must attempt a recovery Start() and emit exactly one
// targeted re-auth notice. Initial-startup auth deferral (toolset never worked
// before) must remain silent. The streak resets on success so a subsequent
// background failure produces a fresh notice.
func TestEmitStartupInfo_RecoveryAuthNoticeEmittedOnce(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}
	authErr := &mcptools.AuthorizationRequiredError{URL: "https://example.test/mcp"}

	inner := &recoveryAuthToolSet{restartErr: authErr}
	root := agent.New("root", "agent",
		agent.WithModel(prov),
		agent.WithToolSets(inner),
	)
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("root"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	var wrapped *tools.StartableToolSet
	for _, ts := range root.ToolSets() {
		if s, ok := ts.(*tools.StartableToolSet); ok {
			wrapped = s
			break
		}
	}
	require.NotNil(t, wrapped, "agent.ToolSets() must wrap the inner toolset in a *tools.StartableToolSet")

	// nopSend discards sidebar events; we inspect agent.DrainWarnings() instead.
	nopSend := func(Event) bool { return true }
	// Mirror EmitStartupInfo\'s non-interactive context so toolsets with OAuth
	// fail fast rather than blocking on a prompt.
	ctx := mcptools.WithoutInteractivePrompts(t.Context())

	// Phase 1: initial startup — inner.Start() succeeds (first call); no recovery
	// notice because the toolset was never previously working.
	rt.emitToolsProgressively(ctx, root, nopSend)
	_ = root.DrainWarnings() // clear any unrelated warnings
	require.True(t, wrapped.IsStarted(), "toolset must be started after initial success")

	// Phase 2: background failure — inner loses its connection (e.g. server-side
	// invalid_token eviction set the live started flag to false).
	inner.started = false

	// First emitToolsProgressively after the background failure: recovery Start()
	// is attempted (Restart returns authErr), and exactly one targeted notice is
	// added to the agent\'s warning queue.
	rt.emitToolsProgressively(ctx, root, nopSend)
	noticesPhase2 := root.DrainWarnings()
	require.Len(t, noticesPhase2, 1,
		"exactly one targeted re-auth notice must be emitted on the first recovery failure")
	assert.Contains(t, noticesPhase2[0], "needs re-authentication",
		"recovery notice must use the targeted re-auth framing, not the generic start-failed message")

	// Dedup: ShouldReportRecoveryFailure was consumed by emitToolsProgressively;
	// a direct call must return false (streak is still active but pending cleared).
	assert.False(t, wrapped.ShouldReportRecoveryFailure(),
		"ShouldReportRecoveryFailure must return false after the first notice was emitted (dedup)")

	// Phase 3: inner recovers — successful Start() (via inner.Start() since
	// wrapped.started==false after failed Restart) resets the recovery streak.
	inner.started = true
	rt.emitToolsProgressively(ctx, root, nopSend)
	_ = root.DrainWarnings()
	require.True(t, wrapped.IsStarted(), "toolset must be re-started after recovery")
	assert.False(t, wrapped.ShouldReportRecoveryFailure(),
		"recovery streak must be reset after a successful Start")

	// Phase 4: background failure again — streak was reset, so a fresh notice
	// is expected (verifies reset-on-success behavior).
	inner.started = false
	rt.emitToolsProgressively(ctx, root, nopSend)
	noticesPhase4 := root.DrainWarnings()
	require.Len(t, noticesPhase4, 1, "fresh failure after streak reset must emit a new notice")
}

// TestEmitStartupInfo_PartialStartStillCountsCodeModeTools is the runtime
// regression test for #3978 degraded startup: when the codemode composite
// starts partially (one inner MCP-like toolset fails), the composite is
// latched as started and emitToolsProgressively must keep listing it — the
// run_tools_with_javascript wrapper is counted in the sidebar's ToolsetInfo
// — while the inner failure still surfaces as a warning.
func TestEmitStartupInfo_PartialStartStillCountsCodeModeTools(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}

	healthy := newStubToolSet(nil, []tools.Tool{{Name: "fetch_url", Parameters: map[string]any{}}}, nil)
	failing := newStubToolSet(errors.New("connection refused"), nil, nil)

	root := agent.New("root", "agent",
		agent.WithModel(prov),
		agent.WithToolSets(codemode.Wrap(healthy, failing)),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("root"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	events := make(chan Event, 32)
	rt.EmitStartupInfo(t.Context(), nil, NewChannelSink(events))
	close(events)

	var toolsetInfos []*ToolsetInfoEvent
	var warning *WarningEvent
	for e := range events {
		switch ev := e.(type) {
		case *ToolsetInfoEvent:
			toolsetInfos = append(toolsetInfos, ev)
		case *WarningEvent:
			warning = ev
		}
	}

	require.NotEmpty(t, toolsetInfos, "expected at least one ToolsetInfo event")
	last := toolsetInfos[len(toolsetInfos)-1]
	assert.False(t, last.Loading, "final ToolsetInfo must report Loading=false")
	assert.Equal(t, 1, last.AvailableTools,
		"the degraded codemode wrapper must still be listed and counted (run_tools_with_javascript)")

	require.NotNil(t, warning, "the failed inner toolset must still surface a warning")
	assert.Contains(t, warning.Message, "start failed")
	assert.Contains(t, warning.Message, "connection refused")
}

// TestConfigureToolsetHandlers_ReachesThroughCodeModeWrapper is the
// regression test for the sigma MCP OAuth bug: an agent with
// code_mode_tools:true wraps all its toolsets in a single codemode
// meta-toolset that is neither Elicitable/OAuthCapable itself nor an
// Unwrapper tools.As can walk through (it hides N inner toolsets, not one).
// configureToolsetHandlers (called once per turn, before Tools()/Start())
// must still reach the inner MCP-like toolset's elicitation handler through
// that wrapper, otherwise its OAuth elicitation request finds no handler
// wired up and defers forever — the toolset never surfaces its
// authorization dialog and registers 0 tools.
func TestConfigureToolsetHandlers_ReachesThroughCodeModeWrapper(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/codemode-model", stream: &mockStream{}}
	inner := &elicitingToolSet{message: "authorize?"}

	root := agent.New("root", "agent",
		agent.WithModel(prov),
		agent.WithToolSets(codemode.Wrap(inner)),
	)
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("root"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	events := make(chan Event, 8)
	rt.configureToolsetHandlers(root, NewChannelSink(events))
	close(events)
	for range events {
	}

	inner.mu.Lock()
	handler := inner.handler
	inner.mu.Unlock()
	require.NotNil(t, handler,
		"configureToolsetHandlers must wire the elicitation handler through the code_mode_tools wrapper")
}

// TestEmitAgentWarnings_OnlyEmitsFailures verifies that emitAgentWarnings
// only surfaces real failures to the user. Recovery is intentionally
// silent: a previously-failed toolset becoming available again does NOT
// produce a follow-up "is now available" notification, because that reads
// as a spurious warning right after the user completes an OAuth dance.
//
// Regression test for: after lazy-init OAuth completed and the toolset
// reconnected, the user saw a notification framed as a warning saying
// "mcp(…) is now available" — noise, not signal.
func TestEmitAgentWarnings_OnlyEmitsFailures(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/m", stream: &mockStream{}}
	root := agent.New("root", "agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("root"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	root.AddToolWarning("toolset_a start failed: connection refused")

	var emitted []*WarningEvent
	rt.emitAgentWarnings(root, EventSinkFunc(func(e Event) {
		if w, ok := e.(*WarningEvent); ok {
			emitted = append(emitted, w)
		}
	}))

	require.Len(t, emitted, 1, "expected exactly one event for one failure (recoveries are silent)")
	w := emitted[0]
	assert.Contains(t, strings.ToLower(w.Message), "failed",
		"failure event must use the failure framing; got: %q", w.Message)
	assert.Contains(t, w.Message, "toolset_a start failed: connection refused")
	assert.NotContains(t, w.Message, "is now available",
		"recovery notices must never be emitted as warnings; got: %q", w.Message)
}

// TestEmitAgentWarnings_NoEventsWhenQueueEmpty verifies that draining an
// agent with no pending warnings emits nothing — in particular, no empty
// "Some toolsets failed to initialize" envelope.
func TestEmitAgentWarnings_NoEventsWhenQueueEmpty(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/m", stream: &mockStream{}}
	root := agent.New("root", "agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("root"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	var emitted int
	rt.emitAgentWarnings(root, EventSinkFunc(func(Event) { emitted++ }))
	assert.Zero(t, emitted, "empty warnings queue must produce zero events")
}

func TestEmitStartupInfo(t *testing.T) {
	t.Parallel()

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

	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("startup-test-agent"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// Create a channel to collect events
	events := make(chan Event, 10)

	// Call EmitStartupInfo
	rt.EmitStartupInfo(t.Context(), nil, NewChannelSink(events))
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
	rt.EmitStartupInfo(t.Context(), nil, NewChannelSink(events2))
	close(events2)

	var collectedEvents2 []Event
	for event := range events2 {
		collectedEvents2 = append(collectedEvents2, event)
	}

	// Should be empty due to deduplication
	require.Empty(t, collectedEvents2, "EmitStartupInfo should not emit duplicate events")
}

func TestEmitStartupInfo_AgentInfoCarriesContextLimit(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}
	root := agent.New("startup-test-agent", "You are a startup test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("startup-test-agent"),
		WithModelStore(mockModelStoreWithLimit{limit: 200_000}))
	require.NoError(t, err)

	events := make(chan Event, 20)
	rt.EmitStartupInfo(t.Context(), nil, NewChannelSink(events))
	close(events)

	var agentInfo *AgentInfoEvent
	for event := range events {
		if e, ok := event.(*AgentInfoEvent); ok {
			agentInfo = e
		}
	}

	require.NotNil(t, agentInfo)
	assert.Equal(t, int64(200_000), agentInfo.ContextLimit)
}

func TestEmitStartupInfo_WithSessionTokenData(t *testing.T) {
	t.Parallel()

	// When restoring a session that already has token data,
	// EmitStartupInfo should emit a TokenUsageEvent with the context limit
	// looked up from the model store so the sidebar can display context %.
	prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}
	root := agent.New("startup-test-agent", "You are a startup test agent",
		agent.WithModel(prov),
		agent.WithDescription("Startup agent"),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("startup-test-agent"),
		WithModelStore(mockModelStoreWithLimit{limit: 200_000}))
	require.NoError(t, err)

	// Create a session with existing token data (simulating session restore)
	sess := session.New()
	sess.InputTokens = 5000
	sess.OutputTokens = 1000

	events := make(chan Event, 20)
	rt.EmitStartupInfo(t.Context(), sess, NewChannelSink(events))
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
	t.Parallel()

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

	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("root"),
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
	rt.EmitStartupInfo(t.Context(), sess, NewChannelSink(events))
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

func TestEmitStartupInfo_LastMessageFinishReason(t *testing.T) {
	t.Parallel()

	// When restoring a session whose last assistant message has a
	// FinishReason, the emitted TokenUsageEvent.LastMessage must carry
	// that FinishReason so the UI can identify the final response.
	prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}
	root := agent.New("root", "agent",
		agent.WithModel(prov),
		agent.WithDescription("Root"),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("root"),
		WithModelStore(mockModelStoreWithLimit{limit: 128_000}))
	require.NoError(t, err)

	sess := session.New()
	sess.InputTokens = 500
	sess.OutputTokens = 200

	sess.Messages = append(sess.Messages, session.Item{
		Message: &session.Message{
			AgentName: "root",
			Message: chat.Message{
				Role:         chat.MessageRoleAssistant,
				Content:      "final answer",
				Cost:         0.02,
				Model:        "test/startup-model",
				FinishReason: chat.FinishReasonStop,
				Usage:        &chat.Usage{InputTokens: 500, OutputTokens: 200},
			},
		},
	})

	events := make(chan Event, 20)
	rt.EmitStartupInfo(t.Context(), sess, NewChannelSink(events))
	close(events)

	var tokenEvent *TokenUsageEvent
	for event := range events {
		if te, ok := event.(*TokenUsageEvent); ok {
			tokenEvent = te
		}
	}

	require.NotNil(t, tokenEvent, "should emit TokenUsageEvent")
	require.NotNil(t, tokenEvent.Usage.LastMessage, "LastMessage should be populated on session restore")
	assert.Equal(t, chat.FinishReasonStop, tokenEvent.Usage.LastMessage.FinishReason)
	assert.Equal(t, "test/startup-model", tokenEvent.Usage.LastMessage.Model)
	assert.InDelta(t, 0.02, tokenEvent.Usage.LastMessage.Cost, 0.0001)
	assert.Equal(t, int64(500), tokenEvent.Usage.LastMessage.InputTokens)
	assert.Equal(t, int64(200), tokenEvent.Usage.LastMessage.OutputTokens)
}

func TestEmitStartupInfo_NilSessionNoTokenEvent(t *testing.T) {
	t.Parallel()

	// When sess is nil, no TokenUsageEvent should be emitted.
	prov := &mockProvider{id: "test/startup-model", stream: &mockStream{}}
	root := agent.New("startup-test-agent", "You are a startup test agent",
		agent.WithModel(prov),
		agent.WithDescription("Startup agent"),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("startup-test-agent"),
		WithModelStore(mockModelStoreWithLimit{limit: 200_000}))
	require.NoError(t, err)

	events := make(chan Event, 20)
	rt.EmitStartupInfo(t.Context(), nil, NewChannelSink(events))
	close(events)

	for event := range events {
		_, isTokenEvent := event.(*TokenUsageEvent)
		assert.False(t, isTokenEvent, "EmitStartupInfo should not emit TokenUsageEvent when session is nil")
	}
}

func TestPermissions_DenyBlocksToolExecution(t *testing.T) {
	t.Parallel()

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

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
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
		Handler: func(ctx context.Context, tc tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("executed"), nil
		},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, agentTools, NewChannelSink(events))
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
	t.Parallel()

	// Test that tools matching allow patterns are auto-approved without --yolo
	permChecker := permissions.NewChecker(&latest.PermissionsConfig{
		Allow: []string{"safe_*"},
	})

	var executed bool
	agentTools := []tools.Tool{{
		Name:       "safe_tool",
		Parameters: map[string]any{},
		Handler: func(ctx context.Context, tc tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
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

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
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
	rt.processToolCalls(t.Context(), sess, calls, agentTools, NewChannelSink(events))
	close(events)

	// The tool should have been executed due to allow pattern
	require.True(t, executed, "expected tool to be auto-approved and executed")
}

func TestPermissions_DenyTakesPriorityOverAllow(t *testing.T) {
	t.Parallel()

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

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
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
		Handler: func(ctx context.Context, tc tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("executed"), nil
		},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, agentTools, NewChannelSink(events))
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
	t.Parallel()

	// Test that session-level deny patterns block tools
	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
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
		Handler: func(ctx context.Context, tc tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("executed"), nil
		},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, agentTools, NewChannelSink(events))
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
	t.Parallel()

	// Test that session-level allow patterns auto-approve tools
	var executed bool
	agentTools := []tools.Tool{{
		Name:       "allowed_tool",
		Parameters: map[string]any{},
		Handler: func(ctx context.Context, tc tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
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

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
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
	rt.processToolCalls(t.Context(), sess, calls, agentTools, NewChannelSink(events))
	close(events)

	require.True(t, executed, "expected tool to be auto-approved by session permissions")
}

func TestSessionPermissions_TakePriorityOverTeamPermissions(t *testing.T) {
	t.Parallel()

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

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
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
		Handler: func(ctx context.Context, tc tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("executed"), nil
		},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, agentTools, NewChannelSink(events))
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
	t.Parallel()

	// Test that rejection reasons are included in the tool error response
	agentTools := []tools.Tool{{
		Name:       "shell",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
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

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
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
		rt.processToolCalls(t.Context(), sess, calls, agentTools, NewChannelSink(events))
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
	t.Parallel()

	// Test that rejection without a reason still works
	agentTools := []tools.Tool{{
		Name:       "shell",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
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

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
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
		rt.processToolCalls(t.Context(), sess, calls, agentTools, NewChannelSink(events))
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
	t.Parallel()

	// root has librarian as sub-agent but NOT planner.
	// planner exists in the team. transfer_task to planner should be rejected.
	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}

	librarian := agent.New("librarian", "Library agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	planner := agent.New("planner", "Planner agent", agent.WithModel(prov))

	agent.WithSubAgents(librarian)(root)

	tm := team.New(team.WithAgents(root, planner, librarian))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
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

	result, err := rt.handleTaskTransfer(t.Context(), sess, toolCall, NewChannelSink(evts), tools.NopRuntime{})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError, "transfer to non-sub-agent should return an error result")
	assert.Contains(t, result.Output, "cannot transfer task to planner")
	assert.Contains(t, result.Output, "librarian")
	assert.Equal(t, "root", rt.CurrentAgentName(t.Context()), "current agent should remain root")
}

func TestTransferTaskAllowsSubAgent(t *testing.T) {
	t.Parallel()

	// Verify that transfer_task to a valid sub-agent is NOT rejected by the validation.
	// We can't fully run the child session without a real model, so we just confirm
	// it gets past validation (it will fail later due to mock stream being empty,
	// which is fine — we only care that it's not blocked by the sub-agent check).
	prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("done").AddStopWithUsage(10, 5).Build()}

	librarian := agent.New("librarian", "Library agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))

	agent.WithSubAgents(librarian)(root)

	tm := team.New(team.WithAgents(root, librarian))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
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

	result, err := rt.handleTaskTransfer(t.Context(), sess, toolCall, NewChannelSink(evts), tools.NopRuntime{})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError, "transfer to valid sub-agent should succeed")
}

// TestTransferTaskPersistsSubSessionOnError covers the case where a sub-agent's
// run loop emits an ErrorEvent — for example because the model stream failed,
// the loop detector fired, or a hook blocked execution. Before the fix in
// runForwarding, an ErrorEvent caused an early return that skipped both
// parent.AddSubSession and the SubSessionCompletedEvent emission, so the
// entire sub-session transcript was silently dropped — invisible to the user,
// invisible to debug tooling that walks session_items.
//
// The fix persists unconditionally: capture the error, drain the channel,
// AddSubSession + emit SubSessionCompleted, then return the error so the
// parent's tool dispatcher still records an error tool result.
func TestTransferTaskPersistsSubSessionOnError(t *testing.T) {
	t.Parallel()
	// Root has a librarian sub-agent whose model always fails to produce a
	// stream. This mirrors the production failure mode where a sub-agent
	// hits an irrecoverable error mid-stream.
	parentProv := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	failingProv := &mockProviderWithError{id: "test/mock-model"}

	librarian := agent.New("librarian", "Library agent", agent.WithModel(failingProv))
	root := agent.New("root", "Root agent", agent.WithModel(parentProv))
	agent.WithSubAgents(librarian)(root)

	tm := team.New(team.WithAgents(root, librarian))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
	sess.NonInteractive = true // mirror --exec mode
	evts := make(chan Event, 128)

	toolCall := tools.ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      "transfer_task",
			Arguments: `{"agent":"librarian","task":"find a book","expected_output":"book title"}`,
		},
	}

	// runForwarding returns an error because the child emitted an ErrorEvent,
	// but only *after* persisting the sub-session.
	_, err = rt.handleTaskTransfer(t.Context(), sess, toolCall, NewChannelSink(evts), tools.NopRuntime{})
	require.Error(t, err, "transfer should surface the sub-session error to the caller")

	// The parent session must now hold a sub-session item — without the fix
	// this would be empty because AddSubSession was skipped on the error path.
	var subSessionItems int
	for _, item := range sess.Messages {
		if item.SubSession != nil {
			subSessionItems++
		}
	}
	assert.Equal(t, 1, subSessionItems,
		"parent session must record the sub-session even when the sub-agent errored — "+
			"otherwise the entire transcript is lost and the failure is invisible to observers")

	// Drain the event channel and assert both required events are present.
	//
	// ErrorEvent must reach the parent sink: runForwarding forwards it
	// unconditionally so the TUI's streamDepth counter stays balanced and
	// the user sees the error context.
	//
	// SubSessionCompletedEvent must fire: the persistence pipeline
	// (PersistenceObserver) writes the sub-session to the store on this
	// event; without it the store never learns the sub-session existed.
	close(evts)
	var sawSubSessionCompleted, sawErrorEvent bool
	for ev := range evts {
		switch ev.(type) {
		case *SubSessionCompletedEvent:
			sawSubSessionCompleted = true
		case *ErrorEvent:
			sawErrorEvent = true
		}
	}
	assert.True(t, sawErrorEvent,
		"ErrorEvent must be forwarded to the parent sink to keep TUI streamDepth balanced")
	assert.True(t, sawSubSessionCompleted,
		"SubSessionCompletedEvent must fire on the error path so observers persist the sub-session")
}

// TestDenyOverridesYoloMode verifies that Deny permissions take precedence over the yolo flag.
func TestDenyOverridesYoloMode(t *testing.T) {
	t.Parallel()

	// Test that deny permissions take precedence over the --yolo flag
	permChecker := permissions.NewChecker(&latest.PermissionsConfig{
		Deny: []string{"dangerous_tool"},
	})

	var executed bool
	agentTools := []tools.Tool{{
		Name:       "dangerous_tool",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
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

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
	require.True(t, sess.ToolsApproved)

	calls := []tools.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "dangerous_tool", Arguments: "{}"},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, agentTools, NewChannelSink(events))
	close(events)

	// With --yolo and Deny/ForceAsk precedence, the tool should NOT execute.
	require.False(t, executed, "expected tool to NOT be executed in --yolo mode because Deny wins")
}

// TestYoloMode_OverridesForceAsk verifies that the yolo flag takes precedence over ForceAsk permissions.
func TestYoloMode_OverridesForceAsk(t *testing.T) {
	t.Parallel()

	// Test that --yolo flag takes precedence over ForceAsk permissions
	permChecker := permissions.NewChecker(&latest.PermissionsConfig{
		Ask: []string{"careful_tool"},
	})

	var executed bool
	agentTools := []tools.Tool{{
		Name:       "careful_tool",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
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

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
	sess.NonInteractive = true // fail fast instead of hanging on askUser if this regresses
	require.True(t, sess.ToolsApproved)

	calls := []tools.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "careful_tool", Arguments: "{}"},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, agentTools, NewChannelSink(events))
	close(events)

	// YOLO overrides ForceAsk: the checker's ForceAsk verdict is bypassed
	// and the tool executes automatically.
	require.True(t, executed, "expected tool to be executed in --yolo mode because YOLO wins over ForceAsk")
}

// TestSessionDenyOverridesYoloMode verifies that session-level Deny permissions take precedence over the yolo flag.
func TestSessionDenyOverridesYoloMode(t *testing.T) {
	t.Parallel()

	// Test that session-level deny takes precedence over the --yolo flag
	var executed bool
	agentTools := []tools.Tool{{
		Name:       "blocked_tool",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
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

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
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
	rt.processToolCalls(t.Context(), sess, calls, agentTools, NewChannelSink(events))
	close(events)

	// With --yolo and Deny/ForceAsk precedence, the tool should NOT execute.
	require.False(t, executed, "expected tool to NOT be executed in --yolo mode because session Deny wins")
}

func TestStripUnsupportedMediaContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		caps     modelinfo.ModelCapabilities // zero value = text-only
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
			name: "strips document parts with image MIME type",
			messages: []chat.Message{
				{
					Role: chat.MessageRoleUser,
					MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeText, Text: "look at this"},
						{
							Type: chat.MessagePartTypeDocument,
							Document: &chat.Document{
								Name:     "photo.png",
								MimeType: "image/png",
								Source:   chat.DocumentSource{InlineData: []byte{0x89, 0x50}},
							},
						},
					},
				},
			},
			want: []chat.Message{
				{
					Role: chat.MessageRoleUser,
					MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeText, Text: "look at this"},
					},
				},
			},
		},
		{
			name: "preserves document parts with non-image MIME type",
			messages: []chat.Message{
				{
					Role: chat.MessageRoleUser,
					MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeText, Text: "here is the doc"},
						{
							Type: chat.MessagePartTypeDocument,
							Document: &chat.Document{
								Name:     "report.pdf",
								MimeType: "application/pdf",
								Source:   chat.DocumentSource{InlineData: []byte{0x25, 0x50}},
							},
						},
					},
				},
			},
			want: []chat.Message{
				{
					Role: chat.MessageRoleUser,
					MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeText, Text: "here is the doc"},
						{
							Type: chat.MessagePartTypeDocument,
							Document: &chat.Document{
								Name:     "report.pdf",
								MimeType: "application/pdf",
								Source:   chat.DocumentSource{InlineData: []byte{0x25, 0x50}},
							},
						},
					},
				},
			},
		},
		{
			name: "preserves document part with nil Document pointer (defensive)",
			messages: []chat.Message{
				{
					Role: chat.MessageRoleUser,
					MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeText, Text: "text"},
						{Type: chat.MessagePartTypeDocument, Document: nil},
					},
				},
			},
			want: []chat.Message{
				{
					Role: chat.MessageRoleUser,
					MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeText, Text: "text"},
						{Type: chat.MessagePartTypeDocument, Document: nil},
					},
				},
			},
		},
		{
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
		{
			name: "strips audio and video documents, preserves supported image",
			caps: modelinfo.CapsWith(true, false, false, false),
			messages: []chat.Message{
				{
					Role: chat.MessageRoleUser,
					MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeText, Text: "listen and watch"},
						{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "clip.wav", MimeType: "audio/wav", Source: chat.DocumentSource{InlineData: []byte{0x52}}}},
						{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "data:image/png;base64,abc"}},
						{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "clip.mp4", MimeType: "video/mp4", Source: chat.DocumentSource{InlineData: []byte{0x00}}}},
					},
				},
			},
			want: []chat.Message{
				{
					Role: chat.MessageRoleUser,
					MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeText, Text: "listen and watch"},
						{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "data:image/png;base64,abc"}},
					},
				},
			},
		},
		{
			name: "retains audio and video when supported",
			caps: modelinfo.CapsWith(false, false, true, true),
			messages: []chat.Message{
				{
					Role: chat.MessageRoleUser,
					MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "clip.mp3", MimeType: "audio/mpeg", Source: chat.DocumentSource{InlineData: []byte{0x49}}}},
						{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "clip.webm", MimeType: "video/webm", Source: chat.DocumentSource{InlineData: []byte{0x1a}}}},
					},
				},
			},
			want: []chat.Message{
				{
					Role: chat.MessageRoleUser,
					MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "clip.mp3", MimeType: "audio/mpeg", Source: chat.DocumentSource{InlineData: []byte{0x49}}}},
						{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "clip.webm", MimeType: "video/webm", Source: chat.DocumentSource{InlineData: []byte{0x1a}}}},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := stripUnsupportedMediaContent(t.Context(), tt.messages, tt.caps)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestResolveSessionAgent_PinnedAgent verifies that resolveSessionAgent returns
// the session-pinned agent when AgentName is set, even though the runtime's
// currentAgent points elsewhere (root). Before the fix, the shared currentAgent
// field was always used, so background sub-agent tasks ran with root's config.
func TestResolveSessionAgent_PinnedAgent(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	worker := agent.New("worker", "Worker agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root, worker))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	assert.Equal(t, "root", rt.CurrentAgentName(t.Context()), "default agent should be root")

	// Session pinned to worker (as run_background_agent does).
	sess := session.New(session.WithAgentName("worker"))

	resolved := rt.resolveSessionAgent(sess)
	assert.Equal(t, "worker", resolved.Name(), "resolveSessionAgent should return pinned agent")
}

// TestResolveSessionAgent_FallsBackToCurrentAgent verifies that when no
// AgentName is set on the session, resolveSessionAgent falls back to the
// runtime's currentAgent (the normal interactive-session path).
func TestResolveSessionAgent_FallsBackToCurrentAgent(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New() // no AgentName
	resolved := rt.resolveSessionAgent(sess)
	assert.Equal(t, "root", resolved.Name(), "should fall back to currentAgent")
}

// TestResolveSessionAgent_InvalidNameFallsBack verifies that if the session's
// AgentName refers to an agent that doesn't exist in the team, we gracefully
// fall back to currentAgent instead of returning nil (which would panic).
func TestResolveSessionAgent_InvalidNameFallsBack(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithAgentName("nonexistent"))
	resolved := rt.resolveSessionAgent(sess)
	require.NotNil(t, resolved, "should never return nil")
	assert.Equal(t, "root", resolved.Name(), "should fall back to currentAgent for unknown AgentName")
}

// TestProcessToolCalls_UsesPinnedAgent verifies that tool-call events emitted by
// processToolCalls carry the pinned agent's name, not root's. Before the fix,
// processToolCalls called r.CurrentAgent() which always returned root for
// background sessions.
func TestProcessToolCalls_UsesPinnedAgent(t *testing.T) {
	t.Parallel()

	var executed bool
	workerTool := tools.Tool{
		Name:       "worker_tool",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			executed = true
			return tools.ResultSuccess("ok"), nil
		},
	}

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	worker := agent.New("worker", "Worker agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root, worker))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	rt.registerDefaultTools()
	assert.Equal(t, "root", rt.CurrentAgentName(t.Context()))

	// Simulate a background session pinned to "worker".
	sess := session.New(
		session.WithUserMessage("go"),
		session.WithToolsApproved(true),
		session.WithAgentName("worker"),
	)

	calls := []tools.ToolCall{{
		ID:       "call-1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "worker_tool", Arguments: "{}"},
	}}

	events := make(chan Event, 32)
	rt.processToolCalls(t.Context(), sess, calls, []tools.Tool{workerTool}, NewChannelSink(events))
	close(events)

	assert.True(t, executed, "worker_tool handler should have been called")

	// Every event emitted must reference "worker", not "root".
	for ev := range events {
		if named, ok := ev.(interface{ GetAgentName() string }); ok {
			assert.Equal(t, "worker", named.GetAgentName(),
				"event %T should reference pinned agent \"worker\", not root", ev)
		}
	}
}

func TestFilterExcludedTools(t *testing.T) {
	t.Parallel()

	allTools := []tools.Tool{
		{Name: "read_skill"},
		{Name: "run_skill"},
		{Name: "shell"},
	}

	t.Run("no exclusions returns all tools", func(t *testing.T) {
		result := filterExcludedTools(allTools, nil)
		assert.Len(t, result, 3)
	})

	t.Run("excludes run_skill", func(t *testing.T) {
		result := filterExcludedTools(allTools, []string{"run_skill"})
		assert.Len(t, result, 2)
		for _, tool := range result {
			assert.NotEqual(t, "run_skill", tool.Name)
		}
	})

	t.Run("excludes multiple tools", func(t *testing.T) {
		result := filterExcludedTools(allTools, []string{"run_skill", "shell"})
		assert.Len(t, result, 1)
		assert.Equal(t, "read_skill", result[0].Name)
	})
}

func TestFilterAllowedTools(t *testing.T) {
	t.Parallel()

	allTools := []tools.Tool{
		{Name: "read_file"},
		{Name: "list_directory"},
		{Name: "shell"},
		{Name: "write_file"},
	}

	t.Run("empty allow-list returns all tools", func(t *testing.T) {
		result := filterAllowedTools(allTools, nil)
		assert.Len(t, result, 4)
	})

	t.Run("exact names", func(t *testing.T) {
		result := filterAllowedTools(allTools, []string{"read_file", "shell"})
		assert.Len(t, result, 2)
		assert.Equal(t, "read_file", result[0].Name)
		assert.Equal(t, "shell", result[1].Name)
	})

	t.Run("glob pattern", func(t *testing.T) {
		result := filterAllowedTools(allTools, []string{"*_file"})
		assert.Len(t, result, 2)
		assert.ElementsMatch(t, []string{"read_file", "write_file"}, []string{result[0].Name, result[1].Name})
	})

	t.Run("no match yields empty", func(t *testing.T) {
		result := filterAllowedTools(allTools, []string{"nonexistent"})
		assert.Empty(t, result)
	})
}

func TestToolNameMatchesAny(t *testing.T) {
	t.Parallel()

	assert.True(t, toolNameMatchesAny("read_file", []string{"read_file"}))
	assert.True(t, toolNameMatchesAny("read_file", []string{"read_*"}))
	assert.True(t, toolNameMatchesAny("read_file", []string{"shell", "read_*"}))
	assert.False(t, toolNameMatchesAny("read_file", []string{"write_*"}))
	assert.False(t, toolNameMatchesAny("read_file", nil))
	// A malformed glob pattern falls back to exact comparison.
	assert.True(t, toolNameMatchesAny("a[b", []string{"a[b"}))
}

// TestSkillSubSessionTools_ScopesAndInjects verifies that a fork-mode skill
// sub-session both restricts inherited tools to the skill's allowed-tools
// list and appends the tools from the skill's assistive toolsets (which are
// exempt from the allow-list).
func TestSkillSubSessionTools_ScopesAndInjects(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	inherited := []tools.Tool{
		{Name: "read_file"},
		{Name: "shell"},
		{Name: "write_file"},
	}
	extraTool := tools.Tool{Name: "fetch"}

	sess := session.New(
		session.WithAllowedTools([]string{"read_file"}),
		session.WithExtraToolSets([]tools.ToolSet{newStubToolSet(nil, []tools.Tool{extraTool}, nil)}),
	)

	result := rt.skillSubSessionTools(t.Context(), sess, root, inherited, NewChannelSink(make(chan Event, 8)))

	names := toolNames(result)
	// read_file kept (allow-listed), shell/write_file filtered out, fetch injected.
	assert.ElementsMatch(t, []string{"read_file", "fetch"}, names)
}

// TestSkillSubSessionTools_NoOpForOrdinarySession verifies that a session
// without AllowedTools/ExtraToolSets is unaffected.
func TestSkillSubSessionTools_NoOpForOrdinarySession(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	inherited := []tools.Tool{{Name: "read_file"}, {Name: "shell"}}
	sess := session.New()

	result := rt.skillSubSessionTools(t.Context(), sess, root, inherited, NewChannelSink(make(chan Event, 8)))
	assert.Equal(t, inherited, result)
}

func TestMergeExcludedTools(t *testing.T) {
	t.Parallel()

	t.Run("both empty", func(t *testing.T) {
		assert.Nil(t, mergeExcludedTools(nil, nil))
	})

	t.Run("parent only", func(t *testing.T) {
		result := mergeExcludedTools([]string{"run_skill"}, nil)
		assert.Equal(t, []string{"run_skill"}, result)
	})

	t.Run("child only", func(t *testing.T) {
		result := mergeExcludedTools(nil, []string{"run_skill"})
		assert.Equal(t, []string{"run_skill"}, result)
	})

	t.Run("deduplicates", func(t *testing.T) {
		result := mergeExcludedTools([]string{"run_skill", "shell"}, []string{"run_skill", "read_skill"})
		assert.Len(t, result, 3)
		assert.ElementsMatch(t, []string{"run_skill", "shell", "read_skill"}, result)
	})
}

func TestRunStream_EmptyMessages_SendUserMessage(t *testing.T) {
	t.Parallel()

	// session.New() defaults to SendUserMessage=true with no messages.
	// With an empty instruction the system prompt is also empty, so
	// GetMessages returns an empty slice.
	// Before the fix, messages[len(messages)-1] panicked with index -1.
	stream := newStreamBuilder().
		AddContent("hello").
		AddStopWithUsage(5, 5).
		Build()

	prov := &mockProvider{id: "test/mock-model", stream: stream}
	root := agent.New("root", "", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New() // SendUserMessage=true, no messages
	sess.Title = "Unit Test"

	// Must not panic.
	evCh := rt.RunStream(t.Context(), sess)
	var events []Event
	for ev := range evCh {
		events = append(events, ev)
	}
	require.NotEmpty(t, events)
}

// TestRunStream_AddEnvironmentInfo_DoesNotPolluteSession pins the
// regression where session_start hook output (the AddEnvironmentInfo
// env block) was persisted as a system message on the session AFTER
// the user's first message had already been added, then surfaced
// verbatim as the [UserMessageEvent] because the runtime relays
// messages[len-1] as the "current" user message.
func TestRunStream_AddEnvironmentInfo_DoesNotPolluteSession(t *testing.T) {
	t.Parallel()

	stream := newStreamBuilder().
		AddContent("reply").
		AddStopWithUsage(5, 5).
		Build()

	prov := &mockProvider{id: "test/mock-model", stream: stream}
	root := agent.New(
		"root", "You are a test agent",
		agent.WithModel(prov),
		agent.WithAddEnvironmentInfo(true),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(),
		tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
		WithWorkingDir(t.TempDir()),
	)
	require.NoError(t, err)

	sess := session.New(
		session.WithUserMessage("hello"),
		session.WithWorkingDir(t.TempDir()),
	)

	evCh := rt.RunStream(t.Context(), sess)
	var events []Event
	for ev := range evCh {
		events = append(events, ev)
	}

	// The persisted transcript must contain only the user message and
	// the assistant reply — no system message smuggled in by the hook.
	var roles []chat.MessageRole
	for _, item := range sess.Messages {
		if item.IsMessage() {
			roles = append(roles, item.Message.Message.Role)
		}
	}
	assert.Equal(t,
		[]chat.MessageRole{chat.MessageRoleUser, chat.MessageRoleAssistant},
		roles,
		"session_start hook output must not be persisted as a session message",
	)

	// The UserMessageEvent must mirror the user's input, not the env
	// info block produced by the hook.
	var userEvts []*UserMessageEvent
	for _, ev := range events {
		if ue, ok := ev.(*UserMessageEvent); ok {
			userEvts = append(userEvts, ue)
		}
	}
	require.Len(t, userEvts, 1)
	assert.Equal(t, "hello", userEvts[0].Message)
	assert.NotContains(t, userEvts[0].Message, "<env>",
		"user_message event must not leak the AddEnvironmentInfo block")
}

// recordingProvider wraps a sequence of mock streams and records the tools
// passed to each CreateChatCompletionStream call.
type recordingProvider struct {
	id      string
	streams []*mockStream
	callIdx int

	mu            sync.Mutex
	recordedCalls [][]tools.Tool // tools passed on each call
}

func (r *recordingProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(r.id) }

func (r *recordingProvider) CreateChatCompletionStream(_ context.Context, _ []chat.Message, toolList []tools.Tool) (chat.MessageStream, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Record the tool names for this call.
	r.recordedCalls = append(r.recordedCalls, slices.Clone(toolList))

	if r.callIdx >= len(r.streams) {
		return newStreamBuilder().AddStopWithUsage(1, 1).Build(), nil
	}
	s := r.streams[r.callIdx]
	r.callIdx++
	return s, nil
}

func (r *recordingProvider) BaseConfig() base.Config { return base.Config{} }
func (r *recordingProvider) MaxTokens() int          { return 0 }

// flappyRuntimeToolSet is a ToolSet+Startable that fails on the first N
// Start() calls and succeeds on all subsequent ones, revealing a new tool
// on success.
type flappyRuntimeToolSet struct {
	mu        sync.Mutex
	attempts  int
	failUntil int // fail while attempts <= failUntil
	newTool   tools.Tool
}

func (f *flappyRuntimeToolSet) Start(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.attempts <= f.failUntil {
		return errors.New("server unavailable")
	}
	return nil
}

func (f *flappyRuntimeToolSet) Stop(_ context.Context) error { return nil }

func (f *flappyRuntimeToolSet) Tools(_ context.Context) ([]tools.Tool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attempts <= f.failUntil {
		return nil, nil
	}
	return []tools.Tool{f.newTool}, nil
}

// TestReprobe_NewToolsAvailableAfterToolCall verifies that when a toolset
// fails to start initially but succeeds after a tool call runs (simulating
// an install step), the reprobe mechanism surfaces the new tool to the model
// on its very next response — within the same user turn.
func TestReprobe_NewToolsAvailableAfterToolCall(t *testing.T) {
	t.Parallel()

	mcpTool := tools.Tool{Name: "mcp_hello", Parameters: map[string]any{}}
	installTool := tools.Tool{
		Name:       "install_mcp",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("installed"), nil
		},
	}

	// Turn 1: model calls install_mcp and keeps going (FinishReasonToolCall → loop continues).
	// Turn 2: model sees mcp_hello in its tool list and stops.
	turn1 := newStreamBuilder().
		AddToolCallName("call_1", "install_mcp").
		AddToolCallArguments("call_1", `{}`).
		AddToolCallStopWithUsage(5, 5).
		Build()
	turn2 := newStreamBuilder().
		AddContent("MCP is now available").
		AddStopWithUsage(3, 3).
		Build()

	flappy := &flappyRuntimeToolSet{newTool: mcpTool, failUntil: 2}
	installTS := newStubToolSet(nil, []tools.Tool{installTool}, nil)

	prov := &recordingProvider{
		id:      "test/mock-model",
		streams: []*mockStream{turn1, turn2},
	}

	root := agent.New("root", "test",
		agent.WithModel(prov),
		agent.WithToolSets(installTS, flappy),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	rt.registerDefaultTools()

	sess := session.New(session.WithUserMessage("Install and use MCP"))
	sess.Title = "reprobe test"
	sess.ToolsApproved = true

	evCh := rt.RunStream(t.Context(), sess)
	var events []Event
	for ev := range evCh {
		events = append(events, ev)
	}

	prov.mu.Lock()
	defer prov.mu.Unlock()

	require.GreaterOrEqual(t, len(prov.recordedCalls), 2, "expected at least 2 model calls")

	// First model call: only install_mcp available (mcp_hello not yet).
	call1Names := toolNames(prov.recordedCalls[0])
	assert.Contains(t, call1Names, "install_mcp", "turn 1 must include install_mcp")
	assert.NotContains(t, call1Names, "mcp_hello", "turn 1 must NOT include mcp_hello before install")
	for _, tool := range prov.recordedCalls[0] {
		assert.False(t, tool.Deferred)
	}

	// Second model call: mcp_hello must be visible and deferred so it preserves
	// the provider's cached tool prefix.
	call2Names := toolNames(prov.recordedCalls[1])
	assert.Contains(t, call2Names, "mcp_hello", "turn 2 must include mcp_hello after reprobe")
	for _, tool := range prov.recordedCalls[1] {
		assert.Equal(t, tool.Name == "mcp_hello", tool.Deferred)
		if tool.Name == "mcp_hello" {
			assert.Equal(t, "call_1", tool.DeferredAtToolCallID)
		}
	}

	// A ToolsetInfo event with the new count must have been emitted during reprobe.
	var toolsetInfoCounts []int
	for _, ev := range events {
		if ti, ok := ev.(*ToolsetInfoEvent); ok {
			toolsetInfoCounts = append(toolsetInfoCounts, ti.AvailableTools)
		}
	}
	assert.Contains(t, toolsetInfoCounts, 2, "ToolsetInfo with count=2 expected after reprobe")
}

// TestReprobe_NoChangeMeansNoExtraEvents verifies that reprobe is a no-op
// (no extra ToolsetInfo events, no panics) when no new tools appear after
// a tool call.
func TestReprobe_NoChangeMeansNoExtraEvents(t *testing.T) {
	t.Parallel()

	staticTool := tools.Tool{
		Name:       "do_thing",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("done"), nil
		},
	}

	stream1 := newStreamBuilder().
		AddToolCallName("c1", "do_thing").
		AddToolCallArguments("c1", `{}`).
		AddStopWithUsage(5, 5).
		Build()

	prov := &recordingProvider{
		id:      "test/mock-model",
		streams: []*mockStream{stream1},
	}

	ts := newStubToolSet(nil, []tools.Tool{staticTool}, nil)
	root := agent.New("root", "test", agent.WithModel(prov), agent.WithToolSets(ts))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	rt.registerDefaultTools()

	sess := session.New(session.WithUserMessage("Do the thing"))
	sess.Title = "no-change reprobe test"
	sess.ToolsApproved = true

	evCh := rt.RunStream(t.Context(), sess)
	var events []Event
	for ev := range evCh {
		events = append(events, ev)
	}

	// Count ToolsetInfo events — reprobe should NOT emit an extra one.
	var counts []int
	for _, ev := range events {
		if ti, ok := ev.(*ToolsetInfoEvent); ok {
			counts = append(counts, ti.AvailableTools)
		}
	}
	// All counts should be 1 (the static tool).
	for _, c := range counts {
		assert.Equal(t, 1, c, "unexpected ToolsetInfo count — reprobe emitted extra event when tools unchanged")
	}
}

func toolNames(ts []tools.Tool) []string {
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = t.Name
	}
	return names
}

// messageRecordingProvider records the chat.Message slices passed to each
// CreateChatCompletionStream call so tests can inspect what the model saw.
type messageRecordingProvider struct {
	id      string
	mu      sync.Mutex
	streams []*mockStream
	callIdx int

	recordedMessages [][]chat.Message // messages passed on each call
}

func (p *messageRecordingProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(p.id) }

func (p *messageRecordingProvider) CreateChatCompletionStream(_ context.Context, msgs []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	snapshot := make([]chat.Message, len(msgs))
	copy(snapshot, msgs)
	p.recordedMessages = append(p.recordedMessages, snapshot)

	if p.callIdx >= len(p.streams) {
		// No stream configured for this call index. Return a plain stop so
		// the caller surfaces this as a test failure via assertion rather
		// than hanging, but also record the unexpected call so the test can
		// detect it with require.Len / require.Equal.
		return newStreamBuilder().AddStopWithUsage(1, 1).Build(), nil
	}
	s := p.streams[p.callIdx]
	p.callIdx++
	return s, nil
}

func (p *messageRecordingProvider) BaseConfig() base.Config { return base.Config{} }
func (p *messageRecordingProvider) MaxTokens() int          { return 0 }

// TestSteer_IdleWindowIsConsumedOnNextTurn verifies that a Steer call made
// while no RunStream is active (i.e. in the idle window between turns) is
// picked up by the very next RunStream iteration. Before the fix the steer
// queue was only drained mid-loop (after tool calls), so a message enqueued
// while idle was stranded and never seen by the model.
func TestSteer_IdleWindowIsConsumedOnNextTurn(t *testing.T) {
	t.Parallel()

	// The model returns a plain-text stop (no tool calls) so we stay in the
	// single-iteration path — this is the exact scenario where the old code
	// would miss the steer message.
	stream := newStreamBuilder().
		AddContent("Got it").
		AddStopWithUsage(5, 3).
		Build()

	prov := &messageRecordingProvider{
		id:      "test/mock-model",
		streams: []*mockStream{stream},
	}

	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// Enqueue a steer message BEFORE calling RunStream — simulating the
	// idle-window race where a Steer call lands between two RunStream
	// invocations.
	err = rt.Steer(t.Context(), QueuedMessage{Content: "urgent: change direction"})
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Do the task"))
	sess.Title = "steer idle-window test"

	evCh := rt.RunStream(t.Context(), sess)
	var events []Event
	for ev := range evCh {
		events = append(events, ev)
	}

	// The run must complete normally (StreamStopped as the last event).
	require.NotEmpty(t, events)
	assert.IsType(t, &StreamStoppedEvent{}, events[len(events)-1],
		"expected StreamStopped as the final event")

	// A UserMessageEvent must have been emitted for the steer message.
	var steerEventFound bool
	for _, ev := range events {
		if ue, ok := ev.(*UserMessageEvent); ok && strings.Contains(ue.Message, "urgent: change direction") {
			steerEventFound = true
			break
		}
	}
	assert.True(t, steerEventFound, "expected a UserMessageEvent for the steer message")

	// --- Session-message assertions ---
	// Find the stored message for the steer injection and verify it was
	// stored as a plain user message with NO system-reminder envelope.
	var steerSessionMsg *session.Message
	for _, item := range sess.Messages {
		if item.IsMessage() &&
			item.Message.Message.Role == chat.MessageRoleUser &&
			strings.Contains(item.Message.Message.Content, "urgent: change direction") {
			steerSessionMsg = item.Message
			break
		}
	}
	require.NotNil(t, steerSessionMsg, "expected a user-role session message containing the steer content")
	assert.Equal(t, "urgent: change direction", steerSessionMsg.Message.Content,
		"top-of-turn steer must be stored as plain content, not wrapped in system-reminder")
	assert.NotContains(t, steerSessionMsg.Message.Content, "<system-reminder>",
		"top-of-turn steer must NOT use the system-reminder envelope")

	// --- Model-call assertions ---
	// Verify the model received a message containing the raw steer content.
	prov.mu.Lock()
	defer prov.mu.Unlock()

	require.NotEmpty(t, prov.recordedMessages, "expected at least one model call")
	firstCallMsgs := prov.recordedMessages[0]

	var foundSteer bool
	for _, m := range firstCallMsgs {
		if strings.Contains(m.Content, "urgent: change direction") {
			// Also assert the model did NOT receive the system-reminder wrapper.
			assert.NotContains(t, m.Content, "<system-reminder>",
				"model must receive raw content, not system-reminder envelope, for top-of-turn steer")
			foundSteer = true
			break
		}
	}
	assert.True(t, foundSteer,
		"model should have received the steer message in its first turn; messages seen: %v",
		firstCallMsgs)
}

// TestSteer_EmptySessionBootstrap verifies that when RunStream is started
// with zero messages in the session but one or more messages already queued
// via Steer, the model receives those messages as its initial context — i.e.
// the run completes normally rather than erroring or producing a vacuous
// response. The behaviour must be identical to a session where those messages
// were added directly via session.WithUserMessage before the call.
func TestSteer_EmptySessionBootstrap(t *testing.T) {
	t.Parallel()

	stream := newStreamBuilder().
		AddContent("Hello from the model").
		AddStopWithUsage(5, 3).
		Build()

	prov := &messageRecordingProvider{
		id:      "test/mock-model",
		streams: []*mockStream{stream},
	}

	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// Enqueue before RunStream — zero messages in the session.
	err = rt.Steer(t.Context(), QueuedMessage{Content: "bootstrap message"})
	require.NoError(t, err)

	// Fresh session with NO messages (SendUserMessage defaults to true but
	// there is nothing to send yet).
	sess := session.New()
	sess.Title = "steer bootstrap test"

	evCh := rt.RunStream(t.Context(), sess)
	var events []Event
	for ev := range evCh {
		events = append(events, ev)
	}

	// The run must complete normally.
	require.NotEmpty(t, events)
	assert.IsType(t, &StreamStoppedEvent{}, events[len(events)-1],
		"expected StreamStopped as the final event; got %T", events[len(events)-1])

	// A UserMessageEvent must have been emitted for the steer message.
	var steerEventFound bool
	for _, ev := range events {
		if ue, ok := ev.(*UserMessageEvent); ok && strings.Contains(ue.Message, "bootstrap message") {
			steerEventFound = true
			break
		}
	}
	assert.True(t, steerEventFound,
		"expected a UserMessageEvent for the bootstrap steer message")

	// --- Session-message assertions ---
	// The stored session message must be plain — no system-reminder envelope.
	var bootstrapMsg *session.Message
	for _, item := range sess.Messages {
		if item.IsMessage() &&
			item.Message.Message.Role == chat.MessageRoleUser &&
			strings.Contains(item.Message.Message.Content, "bootstrap message") {
			bootstrapMsg = item.Message
			break
		}
	}
	require.NotNil(t, bootstrapMsg, "expected a user-role session message for the bootstrap steer")
	assert.Equal(t, "bootstrap message", bootstrapMsg.Message.Content,
		"bootstrap steer must be stored as plain content, not wrapped in system-reminder")
	assert.NotContains(t, bootstrapMsg.Message.Content, "<system-reminder>",
		"bootstrap steer must NOT use the system-reminder envelope")

	// --- Model-call assertions ---
	// The model must have received exactly one call and that call must
	// contain the raw bootstrap message (not wrapped).
	prov.mu.Lock()
	defer prov.mu.Unlock()

	require.Len(t, prov.recordedMessages, 1,
		"expected exactly one model call for the bootstrap turn")

	firstCallMsgs := prov.recordedMessages[0]

	var foundBootstrap bool
	for _, m := range firstCallMsgs {
		if strings.Contains(m.Content, "bootstrap message") {
			// The model must see raw content, not the system-reminder wrapper.
			assert.NotContains(t, m.Content, "<system-reminder>",
				"model must receive raw content, not system-reminder envelope, for bootstrap steer")
			foundBootstrap = true
			break
		}
	}
	assert.True(t, foundBootstrap,
		"model must receive the bootstrap steer message as its first (and only) user turn; messages: %v",
		firstCallMsgs)
}

// hookStream wraps a mockStream and calls onStop synchronously when it
// returns a chunk with FinishReasonStop. This lets a test inject a Steer()
// call at the precise moment the stream signals completion — after the stop
// chunk is read inside fallback.execute but before the mid-loop steer
// drain runs, exercising the end-of-iteration drain at res.Stopped.
type hookStream struct {
	*mockStream

	onStop func()
}

func (h *hookStream) Recv() (chat.MessageStreamResponse, error) {
	resp, err := h.mockStream.Recv()
	if err == nil && len(resp.Choices) > 0 && resp.Choices[0].FinishReason == chat.FinishReasonStop {
		if h.onStop != nil {
			h.onStop()
		}
	}
	return resp, err
}

// steerInjectProvider is a provider whose CreateChatCompletionStream calls a
// hook just before returning the stream. The hook is used to inject a Steer
// message synchronously while the stream response is being prepared — this
// simulates the narrow end-of-iteration race where a Steer() call lands after
// the mid-loop drain but before the res.Stopped break.
type steerInjectProvider struct {
	id      string
	streams []chat.MessageStream
	callIdx int
	onCall  func(callIdx int) // called with the current callIdx before returning
	mu      sync.Mutex
}

func (p *steerInjectProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(p.id) }

func (p *steerInjectProvider) CreateChatCompletionStream(_ context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	idx := p.callIdx
	p.callIdx++
	var s chat.MessageStream
	if idx < len(p.streams) {
		s = p.streams[idx]
	} else {
		s = newStreamBuilder().AddStopWithUsage(1, 1).Build()
	}
	p.mu.Unlock()

	if p.onCall != nil {
		p.onCall(idx)
	}
	return s, nil
}

func (p *steerInjectProvider) BaseConfig() base.Config { return base.Config{} }
func (p *steerInjectProvider) MaxTokens() int          { return 0 }

// TestSteer_EndOfIterationRaceIsConsumedInCurrentRunStream verifies that a
// Steer() call arriving in the narrow window between the mid-loop drain and
// the res.Stopped break is consumed within the same RunStream invocation
// rather than being stranded until the next call.
//
// The hookStream fires the injection synchronously inside Recv() when it
// yields the FinishReasonStop chunk. At that point fallback.execute has
// not yet returned; the steer lands in the queue and is guaranteed to be
// drained by one of the three drain points (mid-loop, end-of-iteration, or
// top-of-next-turn). The test asserts the key invariant: consumed within
// this RunStream (2 model calls, UserMessageEvent present).
func TestSteer_EndOfIterationRaceIsConsumedInCurrentRunStream(t *testing.T) {
	t.Parallel()

	var rt *LocalRuntime // set after NewLocalRuntime

	// Turn 1: plain-text stop. The hookStream injects a Steer() when the
	// stop chunk is returned by Recv(), simulating a race in that window.
	turn1Base := newStreamBuilder().
		AddContent("Here is my response").
		AddStopWithUsage(5, 3).
		Build()
	turn1 := &hookStream{
		mockStream: turn1Base,
		onStop: func() {
			_ = rt.Steer(t.Context(), QueuedMessage{Content: "end-of-iter steer"})
		},
	}
	// Turn 2: the loop re-entered after the steer was consumed; model acks.
	turn2 := newStreamBuilder().
		AddContent("Got your steer, changing direction").
		AddStopWithUsage(5, 3).
		Build()

	prov := &steerInjectProvider{
		id:      "test/mock-model",
		streams: []chat.MessageStream{turn1, turn2},
	}

	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	var err error
	rt, err = NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Do the task"))
	sess.Title = "steer end-of-iter race test"

	evCh := rt.RunStream(t.Context(), sess)
	var events []Event
	for ev := range evCh {
		events = append(events, ev)
	}

	// The run must complete normally.
	require.NotEmpty(t, events)
	assert.IsType(t, &StreamStoppedEvent{}, events[len(events)-1],
		"expected StreamStopped as the final event")

	// The steer message must have been emitted as a UserMessageEvent
	// within this RunStream (not deferred to a future one).
	var steerEventFound bool
	for _, ev := range events {
		if ue, ok := ev.(*UserMessageEvent); ok && strings.Contains(ue.Message, "end-of-iter steer") {
			steerEventFound = true
			break
		}
	}
	assert.True(t, steerEventFound,
		"expected a UserMessageEvent for the end-of-iteration steer within the same RunStream")

	// The provider must have been called twice: once for the original turn
	// and once for the follow-on turn triggered by the steer injection.
	prov.mu.Lock()
	defer prov.mu.Unlock()
	assert.Equal(t, 2, prov.callIdx,
		"expected exactly 2 model calls: original turn + steer follow-on turn")

	// Find the stored session message for the steer and verify it was
	// consumed within this RunStream.
	var steerSessionMsg *session.Message
	for _, item := range sess.Messages {
		if item.IsMessage() &&
			item.Message.Message.Role == chat.MessageRoleUser &&
			strings.Contains(item.Message.Message.Content, "end-of-iter steer") {
			steerSessionMsg = item.Message
			break
		}
	}
	require.NotNil(t, steerSessionMsg, "expected a session message for the end-of-iteration steer")
	// All steer drain sites inject plain user messages; no wrapping occurs
	// regardless of which drain (mid-loop or end-of-iteration) fires first.
	assert.Equal(t, "end-of-iter steer", steerSessionMsg.Message.Content,
		"end-of-iteration steer must be stored as plain content")
	assert.NotContains(t, steerSessionMsg.Message.Content, "<system-reminder>",
		"end-of-iteration steer must NOT use the system-reminder envelope")
}

func TestAppendNewlineToQueuedMessage(t *testing.T) {
	t.Parallel()

	t.Run("plain-text message gets newline appended to Content", func(t *testing.T) {
		sm := QueuedMessage{Content: "hello"}
		got := appendNewlineToQueuedMessage(sm)
		assert.Equal(t, "hello\n", got.Content)
		assert.Nil(t, got.MultiContent)
	})

	t.Run("multi-content message with last part text gets newline on that part", func(t *testing.T) {
		sm := QueuedMessage{
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "https://example.com/img.png"}},
				{Type: chat.MessagePartTypeText, Text: "and this"},
			},
		}
		got := appendNewlineToQueuedMessage(sm)
		// Last part is text — \n appended to it.
		assert.Equal(t, "and this\n", got.MultiContent[1].Text)
		// Image part unchanged.
		assert.Equal(t, chat.MessagePartTypeImageURL, got.MultiContent[0].Type)
	})

	t.Run("multi-content message with last part non-text is returned unchanged", func(t *testing.T) {
		sm := QueuedMessage{
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: "look at this"},
				{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "https://example.com/img.png"}},
			},
		}
		got := appendNewlineToQueuedMessage(sm)
		// Last part is image — non-text parts have their own envelope separator;
		// return unchanged.
		assert.Equal(t, "look at this", got.MultiContent[0].Text)
		assert.Equal(t, chat.MessagePartTypeImageURL, got.MultiContent[1].Type)
	})

	t.Run("multi-content message with no text part is returned unchanged", func(t *testing.T) {
		sm := QueuedMessage{
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "https://example.com/img.png"}},
			},
		}
		got := appendNewlineToQueuedMessage(sm)
		// Image-only messages have no text part to append \n to; they are immune to
		// the run-on tokenisation problem because non-text parts carry their own
		// envelope that acts as a separator. Return unchanged.
		require.Len(t, got.MultiContent, 1)
		assert.Equal(t, chat.MessagePartTypeImageURL, got.MultiContent[0].Type)
	})

	t.Run("original QueuedMessage is not mutated", func(t *testing.T) {
		parts := []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "original"},
		}
		sm := QueuedMessage{MultiContent: parts}
		_ = appendNewlineToQueuedMessage(sm)
		assert.Equal(t, "original", parts[0].Text, "original slice must not be mutated")
	})

	t.Run("plain-text original not mutated", func(t *testing.T) {
		sm := QueuedMessage{Content: "x"}
		_ = appendNewlineToQueuedMessage(sm)
		assert.Equal(t, "x", sm.Content)
	})
}

// TestDrainAndEmitSteered_MultipleMessages verifies that when multiple messages
// are drained from the steer queue, each is emitted as a separate session
// message and non-last messages have "\n" appended to their content, preventing
// the LLM from tokenising adjacent words across message boundaries as a run-on
// string.
func TestDrainAndEmitSteered_MultipleMessages(t *testing.T) {
	t.Parallel()

	// Use a stream that never gets called — we only exercise drainAndEmitSteered directly.
	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// Enqueue three plain-text steer messages before draining.
	require.NoError(t, rt.Steer(t.Context(), QueuedMessage{Content: "first"}))
	require.NoError(t, rt.Steer(t.Context(), QueuedMessage{Content: "second"}))
	require.NoError(t, rt.Steer(t.Context(), QueuedMessage{Content: "third"}))

	sess := session.New()
	events := make(chan Event, 16)

	sr := rt.drainAndEmitSteered(t.Context(), sess, root, NewChannelSink(events))
	close(events)

	assert.True(t, sr.drained, "should report messages were drained")

	// Three separate session messages must have been added.
	var userMsgs []string
	for _, item := range sess.Messages {
		if item.IsMessage() && item.Message.Message.Role == chat.MessageRoleUser {
			userMsgs = append(userMsgs, item.Message.Message.Content)
		}
	}
	require.Len(t, userMsgs, 3, "expected 3 independent user messages")

	// Non-last messages must have "\n" appended; the last must not.
	assert.Equal(t, "first\n", userMsgs[0])
	assert.Equal(t, "second\n", userMsgs[1])
	assert.Equal(t, "third", userMsgs[2])

	// The UserMessageEvent contents must mirror the session messages.
	var eventMsgs []string
	for ev := range events {
		if ue, ok := ev.(*UserMessageEvent); ok {
			eventMsgs = append(eventMsgs, ue.Message)
		}
	}
	require.Len(t, eventMsgs, 3)
	assert.Equal(t, "first\n", eventMsgs[0])
	assert.Equal(t, "second\n", eventMsgs[1])
	assert.Equal(t, "third", eventMsgs[2])
}

// TestDrainAndEmitSteered_MultiContent verifies that the "\n" separator is
// correctly appended to multi-content messages: specifically to the last text
// part rather than the Content field.
func TestDrainAndEmitSteered_MultiContent(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// Two multi-content messages.
	require.NoError(t, rt.Steer(t.Context(), QueuedMessage{
		Content: "first",
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "first"},
			{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "https://example.com/a.png"}},
			{Type: chat.MessagePartTypeText, Text: "first-text-after-img"},
		},
	}))
	require.NoError(t, rt.Steer(t.Context(), QueuedMessage{
		Content: "second",
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "second"},
		},
	}))

	sess := session.New()
	events := make(chan Event, 16)

	sr := rt.drainAndEmitSteered(t.Context(), sess, root, NewChannelSink(events))
	close(events)

	assert.True(t, sr.drained)

	// Two session messages.
	var items []session.Item
	for _, item := range sess.Messages {
		if item.IsMessage() && item.Message.Message.Role == chat.MessageRoleUser {
			items = append(items, item)
		}
	}
	require.Len(t, items, 2)

	// First message: last text part must have "\n" appended.
	firstParts := items[0].Message.Message.MultiContent
	require.Len(t, firstParts, 3)
	assert.Equal(t, "first-text-after-img\n", firstParts[2].Text, "last text part of non-last message should have \\n")
	assert.Equal(t, "first", firstParts[0].Text, "other text parts must be unchanged")

	// Second (last) message: no modification.
	secondParts := items[1].Message.Message.MultiContent
	require.Len(t, secondParts, 1)
	assert.Equal(t, "second", secondParts[0].Text, "last message must not be modified")
}

func TestPostToolHookReceivesToolResult(t *testing.T) {
	t.Parallel()

	var got *hooks.Input
	registry := hooks.NewRegistry()
	require.NoError(t, registry.RegisterBuiltin("capture_post_tool", func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
		inputCopy := *in
		got = &inputCopy
		return nil, nil
	}))

	agentTools := []tools.Tool{{
		Name:       "echo_tool",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("actual tool output"), nil
		},
	}}

	root := agent.New("root", "You are a test agent",
		agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}),
		agent.WithToolSets(newStubToolSet(nil, agentTools, nil)),
		agent.WithHooks(&latest.HooksConfig{
			PostToolUse: []latest.HookMatcherConfig{{
				Matcher: "echo_tool",
				Hooks: []latest.HookDefinition{{
					Type:    "builtin",
					Command: "capture_post_tool",
				}},
			}},
		}),
	)
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	rt.hooksRegistry = registry
	rt.buildHooksExecutors()

	sess := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
	calls := []tools.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "echo_tool", Arguments: `{"message":"hello"}`},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, agentTools, NewChannelSink(events))

	require.NotNil(t, got)
	assert.Equal(t, hooks.EventPostToolUse, got.HookEventName)
	assert.Equal(t, "echo_tool", got.ToolName)
	assert.Equal(t, "call_1", got.ToolUseID)
	assert.Equal(t, "actual tool output", got.ToolResponse)
	assert.False(t, got.ToolError)
	assert.Equal(t, "hello", got.ToolInput["message"])
}

func TestPostToolHookEmitsLifecycleEvents(t *testing.T) {
	t.Parallel()

	registry := hooks.NewRegistry()
	require.NoError(t, registry.RegisterBuiltin("noop_post_tool", func(_ context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
		return nil, nil
	}))

	agentTools := []tools.Tool{{
		Name:       "echo_tool",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("ok"), nil
		},
	}}

	root := agent.New("root", "You are a test agent",
		agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}),
		agent.WithToolSets(newStubToolSet(nil, agentTools, nil)),
		agent.WithHooks(&latest.HooksConfig{
			PostToolUse: []latest.HookMatcherConfig{{
				Matcher: "echo_tool",
				Hooks: []latest.HookDefinition{{
					Type:    "builtin",
					Command: "noop_post_tool",
				}},
			}},
		}),
	)
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	rt.hooksRegistry = registry
	rt.buildHooksExecutors()

	sess := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
	calls := []tools.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "echo_tool", Arguments: `{}`},
	}}

	events := make(chan Event, 10)
	rt.processToolCalls(t.Context(), sess, calls, agentTools, NewChannelSink(events))

	var started *HookStartedEvent
	var finished *HookFinishedEvent
	for len(events) > 0 {
		switch ev := (<-events).(type) {
		case *HookStartedEvent:
			started = ev
		case *HookFinishedEvent:
			finished = ev
		}
	}

	require.NotNil(t, started)
	assert.Equal(t, hooks.EventPostToolUse, started.HookEvent)
	assert.Equal(t, sess.ID, started.SessionID)
	assert.Equal(t, "root", started.AgentName)

	require.NotNil(t, finished)
	assert.Equal(t, hooks.EventPostToolUse, finished.HookEvent)
	assert.Equal(t, sess.ID, finished.SessionID)
	assert.True(t, finished.Allowed)
	assert.Empty(t, finished.Error)
}

func TestElicitationHandler_NonInteractive(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithNonInteractive(true))
	require.NoError(t, err)

	params := &mcp.ElicitParams{
		Message: "Authorize OAuth?",
	}

	result, err := rt.elicitationHandler(t.Context(), params)

	require.NoError(t, err)
	assert.Equal(t, tools.ElicitationActionDecline, result.Action, "non-interactive runtime should decline elicitation")
}

// TestElicitationHandler_Interactive_NoChannel verifies that an interactive
// elicitationHandler call succeeds even when the elicitation bridge has no
// channel configured (no RunStream is active). This used to be a hard
// failure because delivery depended entirely on the bridge; with per-request
// waiters (#3584) the bridge send is best-effort only, and ResumeElicitation
// still routes the response correctly via the waiter registry.
func TestElicitationHandler_Interactive_NoChannel(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	// Default runtime (interactive mode) with no events channel set on the bridge.
	rt, err := NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)

	params := &mcp.ElicitParams{
		Message: "Authorize OAuth?",
	}

	type handlerResult struct {
		result tools.ElicitationResult
		err    error
	}
	done := make(chan handlerResult, 1)
	go func() {
		result, err := rt.elicitationHandler(t.Context(), params)
		done <- handlerResult{result: result, err: err}
	}()

	require.Eventually(t, func() bool { return rt.elicitationWaiters.count() == 1 }, time.Second, time.Millisecond,
		"elicitationHandler must register a waiter even though the bridge has no channel")

	require.NoError(t, rt.ResumeElicitation(t.Context(), tools.ElicitationActionAccept, map[string]any{"ok": true}, ""))

	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.Equal(t, tools.ElicitationActionAccept, got.result.Action)
		assert.Equal(t, map[string]any{"ok": true}, got.result.Content)
	case <-time.After(2 * time.Second):
		t.Fatal("elicitationHandler did not return after ResumeElicitation")
	}
}

// TestRunAgentPersistsSubSessionToStore is the regression test for the
// background-agent spend leak: run_background_agent sub-sessions were added to
// the parent's in-memory object but never written to the session store, so
// their tokens and cost were invisible to anything reading the store ($0
// recorded for work that actually ran). The fix persists the completed
// sub-session directly from runCollecting. This asserts the sub-session row
// reaches the store and carries the worker's recorded usage.
func TestRunAgentPersistsSubSessionToStore(t *testing.T) {
	t.Parallel()

	// Worker produces real output and usage so the sub-session has a transcript.
	workerStream := newStreamBuilder().AddContent("worker done").AddStopWithUsage(100, 50).Build()
	parentProv := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	workerProv := &mockProvider{id: "test/mock-model", stream: workerStream}

	worker := agent.New("worker", "Worker agent", agent.WithModel(workerProv))
	root := agent.New("root", "Root agent", agent.WithModel(parentProv))
	agent.WithSubAgents(worker)(root)

	tm := team.New(team.WithAgents(root, worker))

	store := session.NewInMemorySessionStore()
	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
		WithSessionStore(store),
	)
	require.NoError(t, err)

	// The parent must exist in the store before a sub-session can be linked
	// to it. Use UpdateSession (what OnRunStart calls) so the store holds its
	// own copy of the parent — exactly as in the real flow — rather than
	// aliasing the runtime's in-memory object.
	sess := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	result := rt.RunAgent(t.Context(), agenttool.RunParams{
		AgentName:     "worker",
		Task:          "do something",
		ParentSession: sess,
	})
	require.Empty(t, result.ErrMsg, "RunAgent should succeed")

	// The store's copy of the parent must now contain the sub-session — not
	// just the runtime's in-memory object. Before the fix this was empty,
	// so the background agent's work was recorded nowhere durable.
	stored, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)

	var storedSubSessions []*session.Session
	for _, item := range stored.Messages {
		if item.SubSession != nil {
			storedSubSessions = append(storedSubSessions, item.SubSession)
		}
	}
	require.Len(t, storedSubSessions, 1,
		"the background sub-session must be persisted to the store, not just held in memory")

	// The persisted sub-session must carry the worker's token usage so spend
	// accounting that reads the store sees real numbers, not nothing.
	assert.Positive(t, storedSubSessions[0].InputTokens+storedSubSessions[0].OutputTokens,
		"persisted sub-session must carry the worker's recorded token usage")
}

// TestRunAgentPersistsSubSessionOnError covers the background-agent path
// (runCollecting) when the sub-agent's model stream fails. Before the fix,
// runCollecting returned early on ErrorEvent without calling
// parent.AddSubSession, so the sub-session was silently dropped from the
// parent's in-memory record — invisible to any code that walks session.Messages.
func TestRunAgentPersistsSubSessionOnError(t *testing.T) {
	t.Parallel()

	parentProv := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	failingProv := &mockProviderWithError{id: "test/mock-model"}

	worker := agent.New("worker", "Worker agent", agent.WithModel(failingProv))
	root := agent.New("root", "Root agent", agent.WithModel(parentProv))
	agent.WithSubAgents(worker)(root)

	tm := team.New(team.WithAgents(root, worker))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))

	result := rt.RunAgent(t.Context(), agenttool.RunParams{
		AgentName:     "worker",
		Task:          "do something",
		ParentSession: sess,
	})

	require.NotEmpty(t, result.ErrMsg, "RunAgent should surface the sub-session error")

	// The parent session must hold a sub-session record even though the
	// sub-agent errored — without the fix AddSubSession was skipped and
	// the entire partial transcript was lost.
	var subSessionItems int
	for _, item := range sess.Messages {
		if item.SubSession != nil {
			subSessionItems++
		}
	}
	assert.Equal(t, 1, subSessionItems,
		"parent session must record the sub-session even when the background agent errored")
}

// TestRunAgentImmediateFailureEmitsNoZeroUsageEvent guards runCollecting's
// final authoritative snapshot: a background child that fails before
// recording any usage or cost (here the provider errors on stream creation)
// must not surface a zero-usage TokenUsageEvent — it would add an empty
// sub-session row to the UI and clobber per-agent context accounting. The
// guard keys off the child's recorded usage alone and skips the event — and
// the context-limit lookup — before any model resolution; the model store
// still resolves a real limit so a wrongly emitted event would carry one.
func TestRunAgentImmediateFailureEmitsNoZeroUsageEvent(t *testing.T) {
	t.Parallel()

	parentProv := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	failingProv := &mockProviderWithError{id: "test/mock-model"}

	worker := agent.New("worker", "Worker agent", agent.WithModel(failingProv))
	root := agent.New("root", "Root agent", agent.WithModel(parentProv))
	agent.WithSubAgents(worker)(root)

	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker)),
		WithSessionCompaction(false),
		WithModelStore(mockModelStoreWithCostAndLimit{limit: 100_000, cost: modelsdev.Cost{Input: 10, Output: 20}}))
	require.NoError(t, err)

	var forwarded []Event
	rt.OnBackgroundEvent(func(event Event) { forwarded = append(forwarded, event) })

	sess := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
	result := rt.RunAgent(t.Context(), agenttool.RunParams{
		AgentName:     "worker",
		Task:          "do something",
		ParentSession: sess,
	})
	require.NotEmpty(t, result.ErrMsg, "RunAgent should surface the sub-session error")

	assert.Empty(t, forwarded,
		"a child that failed before any billed work must not emit a zero-usage snapshot")
}

// mockModelStoreWithCostAndLimit prices tokens so usage snapshots carry a
// nonzero cost, in addition to a resolvable context limit.
type mockModelStoreWithCostAndLimit struct {
	ModelStore

	limit int
	cost  modelsdev.Cost
}

func (m mockModelStoreWithCostAndLimit) GetModel(_ context.Context, _ modelsdev.ID) (*modelsdev.Model, error) {
	return &modelsdev.Model{Limit: modelsdev.Limit{Context: m.limit}, Cost: &m.cost}, nil
}

// lastAttachedSubSession returns the most recent sub-session recorded on sess.
func lastAttachedSubSession(sess *session.Session) *session.Session {
	var sub *session.Session
	for _, item := range sess.Messages {
		if item.SubSession != nil {
			sub = item.SubSession
		}
	}
	return sub
}

// TestRunAgentForwardsTokenUsageOutOfBand verifies runCollecting surfaces the
// background sub-session's TokenUsageEvents through OnBackgroundEvent — the
// out-of-band path that lets the TUI keep per-agent context accounting for
// background agents — tagged with the sub-session id and the worker's name,
// and forwards nothing else. The last event is the authoritative final
// snapshot runCollecting emits before attaching the child, and must match
// the child's final recorded state.
func TestRunAgentForwardsTokenUsageOutOfBand(t *testing.T) {
	t.Parallel()

	workerStream := newStreamBuilder().AddContent("worker done").AddStopWithUsage(100, 50).Build()
	parentProv := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	workerProv := &mockProvider{id: "test/mock-model", stream: workerStream}

	worker := agent.New("worker", "Worker agent", agent.WithModel(workerProv))
	root := agent.New("root", "Root agent", agent.WithModel(parentProv))
	agent.WithSubAgents(worker)(root)

	tm := team.New(team.WithAgents(root, worker))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false),
		WithModelStore(mockModelStoreWithCostAndLimit{limit: 100_000, cost: modelsdev.Cost{Input: 10, Output: 20}}))
	require.NoError(t, err)

	var forwarded []Event
	rt.OnBackgroundEvent(func(event Event) { forwarded = append(forwarded, event) })

	sess := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
	result := rt.RunAgent(t.Context(), agenttool.RunParams{
		AgentName:     "worker",
		Task:          "do something",
		ParentSession: sess,
	})
	require.Empty(t, result.ErrMsg, "RunAgent should succeed")

	require.NotEmpty(t, forwarded, "the background task's token usage must be forwarded out-of-band")
	for _, event := range forwarded {
		usage, ok := event.(*TokenUsageEvent)
		require.Truef(t, ok, "only TokenUsageEvents may be forwarded, got %T", event)
		assert.Equal(t, "worker", usage.AgentName, "usage is attributed to the background agent")
		assert.NotEqual(t, sess.ID, usage.SessionID, "usage carries the sub-session id, not the parent's")
		assert.NotEmpty(t, usage.SessionID)
	}

	// Once attached, the child is live-attached and the parent's own events
	// exclude its cost, so the last out-of-band snapshot must already carry
	// the child's final accounting: cumulative tokens, cost (100×$10 +
	// 50×$20 per 1M tokens), and the resolved context limit.
	sub := lastAttachedSubSession(sess)
	require.NotNil(t, sub)
	last := forwarded[len(forwarded)-1].(*TokenUsageEvent)
	require.NotNil(t, last.Usage)
	assert.Equal(t, sub.ID, last.SessionID)
	assert.Equal(t, int64(150), last.Usage.ContextLength, "final snapshot carries the worker's cumulative usage")
	assert.InDelta(t, 0.002, last.Usage.Cost, 1e-9, "final snapshot carries the worker's final cost")
	assert.InDelta(t, sub.OwnCost()+sub.EmbeddedSubSessionCost(), last.Usage.Cost, 1e-9,
		"the snapshot's cost is exactly what the attached child reports for itself")
	assert.Equal(t, int64(100_000), last.Usage.ContextLimit, "final snapshot resolves the worker's context limit")
}

// TestRunAgentEmitsFinalUsageOnCancellation is the regression test for the
// background cancellation accounting gap: when the task's context is
// cancelled, runCollecting's forwarding loop breaks and blindly drains the
// remaining events — including the TokenUsageEvent carrying the child's
// recorded usage. Because the child is then attached live to the parent
// (excluded from the parent's own snapshots), nothing would ever surface its
// cost. runCollecting must emit one final authoritative snapshot before
// attaching the child.
func TestRunAgentEmitsFinalUsageOnCancellation(t *testing.T) {
	t.Parallel()

	// The worker streams content, then calls a tool that cancels the task
	// context — strictly after the turn's usage was recorded and its
	// TokenUsageEvent buffered. onContent (below) blocks until the
	// cancellation is visible, so the forwarding loop deterministically
	// breaks before reaching that TokenUsageEvent.
	ctx, cancel := context.WithCancel(t.Context())
	contentSeen := make(chan struct{})
	cancelled := make(chan struct{})

	workerTools := []tools.Tool{{
		Name:       "cancel_task",
		Parameters: map[string]any{},
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			<-contentSeen
			cancel()
			close(cancelled)
			return tools.ResultSuccess("cancelling"), nil
		},
	}}
	workerStream := newStreamBuilder().
		AddContent("partial").
		AddToolCallName("call-1", "cancel_task").
		AddToolCallArguments("call-1", "{}").
		AddToolCallStopWithUsage(100, 50).
		Build()

	worker := agent.New("worker", "Worker agent",
		agent.WithModel(&mockProvider{id: "test/mock-model", stream: workerStream}),
		agent.WithToolSets(newStubToolSet(nil, workerTools, nil)),
	)
	root := agent.New("root", "Root agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}))
	agent.WithSubAgents(worker)(root)

	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker)),
		WithSessionCompaction(false),
		WithModelStore(mockModelStoreWithCostAndLimit{limit: 100_000, cost: modelsdev.Cost{Input: 10, Output: 20}}))
	require.NoError(t, err)

	var forwarded []*TokenUsageEvent
	rt.OnBackgroundEvent(func(event Event) {
		if usage, ok := event.(*TokenUsageEvent); ok {
			forwarded = append(forwarded, usage)
		}
	})

	sess := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
	var once sync.Once
	rt.RunAgent(ctx, agenttool.RunParams{
		AgentName:     "worker",
		Task:          "do something",
		ParentSession: sess,
		OnContent: func(string) {
			once.Do(func() {
				close(contentSeen)
				<-cancelled
			})
		},
	})

	// The cancelled child still attached with the turn it completed.
	sub := lastAttachedSubSession(sess)
	require.NotNil(t, sub, "the cancelled child must still be attached to the parent")
	require.Equal(t, int64(150), sub.InputTokens+sub.OutputTokens, "the child recorded the turn before cancellation")

	require.NotEmpty(t, forwarded,
		"a final authoritative snapshot must be emitted even though the loop broke on cancellation")
	last := forwarded[len(forwarded)-1]
	require.NotNil(t, last.Usage)
	assert.Equal(t, sub.ID, last.SessionID)
	assert.Equal(t, "worker", last.AgentName)
	assert.Equal(t, int64(150), last.Usage.ContextLength, "final snapshot carries the usage recorded before cancellation")
	assert.InDelta(t, sub.OwnCost()+sub.EmbeddedSubSessionCost(), last.Usage.Cost, 1e-9,
		"the snapshot's cost is exactly what the attached child reports for itself")
	assert.Positive(t, last.Usage.Cost, "cost is mandatory on the final snapshot")
	assert.Equal(t, int64(100_000), last.Usage.ContextLimit, "context limit resolves even after cancellation")
}

// TestReasoningOnlyTurnEmitsWarning is a regression test for
// https://github.com/docker/docker-agent/issues/3145. A thinking-mode model
// (e.g. Qwen3 via an openai_chatcompletions provider) can stream only reasoning
// tokens and then stop with no content and no tool calls. recordAssistantMessage
// skips that empty turn, which previously left the user staring at silence. The
// runtime must instead surface a Warning explaining that the model produced only
// reasoning, and must still not persist an assistant message.
func TestReasoningOnlyTurnEmitsWarning(t *testing.T) {
	stream := newStreamBuilder().
		AddReasoning("The user wants the file list. ").
		AddReasoning("I should call list_directory.").
		AddStopWithUsage(10, 25).
		Build()

	sess := session.New(session.WithUserMessage("List the files"))
	events := runSession(t, sess, stream)

	var warn *WarningEvent
	assistantPersisted := false
	for _, ev := range events {
		switch e := ev.(type) {
		case *WarningEvent:
			warn = e
		case *MessageAddedEvent:
			if e.Message != nil && e.Message.Message.Role == chat.MessageRoleAssistant {
				assistantPersisted = true
			}
		}
	}

	require.NotNil(t, warn, "expected a Warning event for a reasoning-only turn")
	assert.Contains(t, warn.Message, "only reasoning")
	assert.False(t, assistantPersisted,
		"a reasoning-only turn must not persist an assistant message")
}

// TestEmptyTurnEmitsWarning verifies that a wholly empty turn (no content, no
// reasoning, no tool calls) - e.g. a rate-limited or token-capped provider that
// returns a bare [DONE] stream, the dominant cause of empty replies on OVHcloud's
// free tier in #3145 - is surfaced as a Warning instead of terminating silently.
func TestEmptyTurnEmitsWarning(t *testing.T) {
	stream := newStreamBuilder().
		AddStopWithUsage(5, 0).
		Build()

	sess := session.New(session.WithUserMessage("hello"))
	events := runSession(t, sess, stream)

	var warn *WarningEvent
	for _, ev := range events {
		if w, ok := ev.(*WarningEvent); ok {
			warn = w
		}
	}

	require.NotNil(t, warn, "expected a Warning event for an empty turn")
	assert.Contains(t, warn.Message, "empty response")
}

// TestEmptyTrailingTurnAfterToolCallsIsSilent verifies that a natural empty
// stop immediately following a tool-call turn does NOT emit the empty-response
// warning. This is the common fork-skill tail (the last action is a tool call
// such as commit/open-pr, then the model stops with nothing left to say); the
// scary rate-limit / token-cap warning there is pure noise. Turns that stop
// empty WITHOUT preceding tool work still warn (covered by TestEmptyTurnEmitsWarning).
func TestEmptyTrailingTurnAfterToolCallsIsSilent(t *testing.T) {
	t.Parallel()

	doneTool := tools.Tool{
		Name:       "do_thing",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("done"), nil
		},
	}

	// Turn 1: model calls the tool and keeps going.
	// Turn 2: model stops with no content and no tool calls (benign tail).
	turn1 := newStreamBuilder().
		AddToolCallName("c1", "do_thing").
		AddToolCallArguments("c1", `{}`).
		AddToolCallStopWithUsage(5, 5).
		Build()
	turn2 := newStreamBuilder().
		AddStopWithUsage(3, 0).
		Build()

	prov := &recordingProvider{
		id:      "test/mock-model",
		streams: []*mockStream{turn1, turn2},
	}

	ts := newStubToolSet(nil, []tools.Tool{doneTool}, nil)
	root := agent.New("root", "test", agent.WithModel(prov), agent.WithToolSets(ts))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	rt.registerDefaultTools()

	sess := session.New(session.WithUserMessage("Do the thing"))
	sess.Title = "empty trailing turn test"
	sess.ToolsApproved = true

	evCh := rt.RunStream(t.Context(), sess)
	var events []Event
	for ev := range evCh {
		events = append(events, ev)
	}

	for _, ev := range events {
		if w, ok := ev.(*WarningEvent); ok {
			assert.NotContains(t, w.Message, "empty response",
				"a benign empty stop right after tool calls must not emit the empty-response warning")
		}
	}
}

// TestEmptyTurnWarning exercises the pure classification helper directly,
// covering each branch without spinning up a full run loop.
func TestEmptyTurnWarning(t *testing.T) {
	t.Parallel()

	t.Run("benign stop after tool calls is silent", func(t *testing.T) {
		res := streamResult{Stopped: true}
		got := emptyTurnWarning(res, true, "test/model", chat.FinishReasonStop)
		assert.Empty(t, got)
	})

	t.Run("reasoning-only turn warns about reasoning", func(t *testing.T) {
		res := streamResult{Stopped: true, ReasoningContent: "thinking..."}
		got := emptyTurnWarning(res, false, "test/model", chat.FinishReasonStop)
		assert.Contains(t, got, "only reasoning")
	})

	t.Run("benign check takes precedence over reasoning-only", func(t *testing.T) {
		// A reasoning-only turn that naturally stops right after tool work is
		// still benign: the tool calls were the turn's real output.
		res := streamResult{Stopped: true, ReasoningContent: "thinking..."}
		got := emptyTurnWarning(res, true, "test/model", chat.FinishReasonStop)
		assert.Empty(t, got)
	})

	t.Run("empty turn without prior tool calls warns", func(t *testing.T) {
		res := streamResult{Stopped: true}
		got := emptyTurnWarning(res, false, "test/model", chat.FinishReasonStop)
		assert.Contains(t, got, "empty response")
	})

	t.Run("length finish after tool calls still warns (truncated, not benign)", func(t *testing.T) {
		// A "length" stop means the reply was cut off by the output token
		// limit; even after tool calls this is a real truncation the user
		// should see, so it must NOT be silenced by the benign case.
		res := streamResult{Stopped: true}
		got := emptyTurnWarning(res, true, "test/model", chat.FinishReasonLength)
		assert.Contains(t, got, "empty response")
	})

	t.Run("non-stop empty turn after tool calls still warns", func(t *testing.T) {
		res := streamResult{Stopped: false}
		got := emptyTurnWarning(res, true, "test/model", chat.FinishReasonNull)
		assert.Contains(t, got, "empty response")
	})
}
