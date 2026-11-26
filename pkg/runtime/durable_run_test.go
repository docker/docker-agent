package runtime

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
)

// transcriptProvider keeps the conversation of every request it receives and
// answers each with the stream answer returns.
type transcriptProvider struct {
	mu       sync.Mutex
	requests [][]chat.Message
	answer   func(ctx context.Context) chat.MessageStream
}

func (p *transcriptProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero("test/mock-model") }

func (p *transcriptProvider) CreateChatCompletionStream(ctx context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var conversation []chat.Message
	for _, msg := range messages {
		if msg.Role != chat.MessageRoleSystem {
			conversation = append(conversation, msg)
		}
	}
	p.requests = append(p.requests, conversation)
	if p.answer != nil {
		return p.answer(ctx), nil
	}
	return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(), nil
}

func (p *transcriptProvider) BaseConfig() base.Config { return base.Config{} }
func (p *transcriptProvider) MaxTokens() int          { return 0 }

func (p *transcriptProvider) received() [][]chat.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests
}

// cutOffStream sends the start of an answer and then hangs until its request
// is cancelled, like a model call that is in flight when its host stops.
type cutOffStream struct {
	cancelled <-chan struct{}
	sent      bool
}

func (s *cutOffStream) Recv() (chat.MessageStreamResponse, error) {
	if !s.sent {
		s.sent = true
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "The answer is"}}}}, nil
	}
	<-s.cancelled
	return chat.MessageStreamResponse{}, context.Canceled
}

func (s *cutOffStream) Close() {}

// probeToolSet reports when the runtime first asks an agent for its tools.
type probeToolSet struct {
	once    sync.Once
	onTools func()
}

func (p *probeToolSet) Tools(context.Context) ([]tools.Tool, error) {
	p.once.Do(p.onTools)
	return nil, nil
}

func durableStore(t *testing.T) session.Store {
	t.Helper()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "session.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func durableRuntime(t *testing.T, store session.Store, prov *transcriptProvider, toolSets ...tools.ToolSet) *LocalRuntime {
	t.Helper()
	root := agent.New("root", "You are a test agent", agent.WithModel(prov), agent.WithToolSets(toolSets...))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	return rt
}

// leftBehind stores what a process that died mid-run leaves: the record of a
// run startedBy an agent, and a transcript with the prompt and what followed.
func leftBehind(t *testing.T, store session.Store, startedBy string, transcript ...*session.Message) *session.Session {
	t.Helper()
	ctx := t.Context()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))
	require.NoError(t, store.(session.RunStore).BeginRun(ctx, sess.ID, startedBy, session.UserMessage("build it")))
	for _, msg := range transcript {
		_, err := store.AddMessage(ctx, sess.ID, msg)
		require.NoError(t, err)
	}
	return reload(t, store, sess.ID)
}

// answerOf is an answer the model of an agent stopped on, as the run loop
// records it.
func answerOf(agentName, content string) *session.Message {
	return session.NewAgentMessage(agentName, &chat.Message{
		Role: chat.MessageRoleAssistant, Content: content, FinishReason: chat.FinishReasonStop, Model: "test/mock-model",
	})
}

func reload(t *testing.T, store session.Store, id string) *session.Session {
	t.Helper()
	sess, err := store.GetSession(t.Context(), id)
	require.NoError(t, err)
	return sess
}

func recordedRuns(t *testing.T, store session.Store) []session.Run {
	t.Helper()
	runs, err := store.(session.RunStore).ListRuns(t.Context())
	require.NoError(t, err)
	return runs
}

func drain(events <-chan Event) []Event {
	var all []Event
	for ev := range events {
		all = append(all, ev)
	}
	return all
}

// transcriptOf summarises messages as "role: content" lines.
func transcriptOf[M any](messages []M, message func(M) chat.Message) []string {
	lines := make([]string, 0, len(messages))
	for _, m := range messages {
		msg := message(m)
		lines = append(lines, string(msg.Role)+": "+msg.Content)
	}
	return lines
}

func stored(t *testing.T, store session.Store, id string) []string {
	t.Helper()
	return transcriptOf(reload(t, store, id).GetAllMessages(), func(m session.Message) chat.Message { return m.Message })
}

func sent(request []chat.Message) []string {
	return transcriptOf(request, func(m chat.Message) chat.Message { return m })
}

func TestRunIsOnRecordWithItsPromptBeforeToolsStart(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	sess := session.New(session.WithUserMessage("build it"))

	var runsAtToolStart []session.Run
	var transcriptAtToolStart []string
	probe := &probeToolSet{onTools: func() {
		runsAtToolStart = recordedRuns(t, store)
		transcriptAtToolStart = stored(t, store, sess.ID)
	}}
	rt := durableRuntime(t, store, &transcriptProvider{}, probe)

	drain(rt.RunStream(t.Context(), sess))

	assert.Equal(t, []session.Run{{SessionID: sess.ID, Agent: "root", Current: "root"}}, runsAtToolStart)
	assert.Equal(t, []string{"user: build it"}, transcriptAtToolStart)
	assert.Equal(t, []string{"user: build it", "assistant: done"}, stored(t, store, sess.ID), "the prompt is stored once")
	assert.Empty(t, recordedRuns(t, store), "a run that ended leaves no record")
}

// cutOffMidAnswer runs a prompt and cancels the run with cause once the model
// has started answering.
func cutOffMidAnswer(t *testing.T, store session.Store, cause error) *session.Session {
	t.Helper()
	prov := &transcriptProvider{answer: func(ctx context.Context) chat.MessageStream { return &cutOffStream{cancelled: ctx.Done()} }}
	rt := durableRuntime(t, store, prov)
	sess := session.New(session.WithUserMessage("build it"))

	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	for ev := range rt.RunStream(ctx, sess) {
		if _, answering := ev.(*AgentChoiceEvent); answering {
			cancel(cause)
		}
	}
	return sess
}

func TestCancelledRunLeavesNoRecord(t *testing.T) {
	t.Parallel()
	store := durableStore(t)

	cutOffMidAnswer(t, store, nil)

	assert.Empty(t, recordedRuns(t, store))
}

func TestSuspendedRunIsContinuedWithoutNewInput(t *testing.T) {
	t.Parallel()
	store := durableStore(t)

	sess := cutOffMidAnswer(t, store, ErrSuspended)

	assert.Equal(t, []session.Run{{SessionID: sess.ID, Agent: "root", Current: "root", Suspended: true}}, recordedRuns(t, store))

	// A later process picks the run up from the store alone.
	prov := &transcriptProvider{}
	events := drain(durableRuntime(t, store, prov).ContinueStream(t.Context(), reload(t, store, sess.ID)))

	requests := prov.received()
	require.Len(t, requests, 1)
	assert.Equal(t, []string{"user: build it"}, sent(requests[0]), "the cut-off answer is not sent back to the model")
	assert.Equal(t, []string{"user: build it", "assistant: The answer is", "assistant: done"}, stored(t, store, sess.ID))
	assert.False(t, hasEventType(t, events, &UserMessageEvent{}), "a continued run announces no prompt")
	assert.Empty(t, recordedRuns(t, store))
}

func TestRunSuspendedDuringAToolCallIsContinued(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	running := make(chan struct{})
	build := tools.Tool{
		Name:       "build",
		Parameters: map[string]any{},
		Handler: func(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			close(running)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	withBuildTool := func(model *transcriptProvider) *LocalRuntime {
		root := agent.New("root", "You are a test agent", agent.WithModel(model), agent.WithTools(build))
		rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
			WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)
		return rt
	}
	model := &transcriptProvider{answer: func(context.Context) chat.MessageStream {
		return newStreamBuilder().
			AddToolCallName("call_1", "build").
			AddToolCallArguments("call_1", `{}`).
			AddToolCallStopWithUsage(1, 1).
			Build()
	}}
	sess := session.New(session.WithUserMessage("build it"), session.WithToolsApproved(true))

	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	for ev := range withBuildTool(model).RunStream(ctx, sess) {
		if _, called := ev.(*ToolCallEvent); called {
			<-running
			cancel(ErrSuspended)
		}
	}

	assert.Equal(t, []session.Run{{SessionID: sess.ID, Agent: "root", Current: "root", Suspended: true}}, recordedRuns(t, store))

	model = &transcriptProvider{}
	drain(withBuildTool(model).ContinueStream(t.Context(), reload(t, store, sess.ID)))

	requests := model.received()
	require.Len(t, requests, 1)
	result := requests[0][len(requests[0])-1]
	assert.Equal(t, "call_1", result.ToolCallID)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content, "interrupted", "the tool is not run again: the model is told it was cut off")
	assert.Empty(t, recordedRuns(t, store))
}

func TestContinueStreamGivesAnInterruptedToolCallItsResult(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	sess := leftBehind(t, store, "root", &session.Message{Message: chat.Message{
		Role:         chat.MessageRoleAssistant,
		FinishReason: chat.FinishReasonToolCalls,
		ToolCalls:    []tools.ToolCall{{ID: "call_1", Type: "function", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"make"}`}}},
	}})
	prov := &transcriptProvider{}

	drain(durableRuntime(t, store, prov).ContinueStream(t.Context(), sess))

	requests := prov.received()
	require.Len(t, requests, 1)
	require.Len(t, requests[0], 3)
	result := requests[0][2]
	assert.Equal(t, chat.MessageRoleTool, result.Role)
	assert.Equal(t, "call_1", result.ToolCallID)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content, "interrupted")
	assert.Equal(t,
		[]string{"user: build it", "assistant: ", "tool: " + session.InterruptedToolResult, "assistant: done"},
		stored(t, store, sess.ID), "the result is in the transcript: the call is over, not in flight")
	assert.Empty(t, recordedRuns(t, store))
}

func TestContinueStreamEndsARunThatHadAlreadyAnswered(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	sess := leftBehind(t, store, "root", answerOf("root", "built"))
	prov := &transcriptProvider{}

	events := drain(durableRuntime(t, store, prov).ContinueStream(t.Context(), sess))

	assert.Empty(t, prov.received(), "the model is not asked to answer its own answer")
	stopped, ok := events[len(events)-1].(*StreamStoppedEvent)
	require.True(t, ok)
	assert.Equal(t, turnEndReasonNormal, stopped.Reason)
	assert.Empty(t, recordedRuns(t, store))
}

func TestContinueStreamResumesAtWhatFollowsAnAnswer(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	// The run was cut off after root answered, before its forced handoff.
	sess := leftBehind(t, store, "root", answerOf("root", "extracted"))
	rootModel, summarizerModel := &transcriptProvider{}, &transcriptProvider{}
	summarizer := agent.New("summarizer", "You summarize", agent.WithModel(summarizerModel))
	root := agent.New("root", "You extract", agent.WithModel(rootModel), agent.WithForceHandoff(summarizer))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, summarizer)),
		WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	drain(rt.ContinueStream(t.Context(), sess))

	assert.Empty(t, rootModel.received(), "the agent that had answered is not asked again")
	require.Len(t, summarizerModel.received(), 1, "the handoff its answer was to trigger still happens")
	assert.Equal(t, []string{"user: build it", "assistant: extracted", "assistant: done"}, stored(t, store, sess.ID))
	assert.Empty(t, recordedRuns(t, store))
}

func TestContinueStreamRefusesARunItCannotContinue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		left func(t *testing.T, store session.Store) *session.Session
	}{
		{
			name: "the run keeps crashing",
			left: func(t *testing.T, store session.Store) *session.Session {
				t.Helper()
				sess := leftBehind(t, store, "root")
				// Every earlier attempt to continue the run died too.
				for range maxRunCrashes - 1 {
					_, err := store.(session.RunStore).ContinueRun(t.Context(), sess.ID)
					require.NoError(t, err)
				}
				return sess
			},
		},
		{
			name: "another agent started the run",
			left: func(t *testing.T, store session.Store) *session.Session {
				t.Helper()
				return leftBehind(t, store, "worker")
			},
		},
		{
			name: "the conversation had moved to another agent",
			left: func(t *testing.T, store session.Store) *session.Session {
				t.Helper()
				sess := leftBehind(t, store, "root")
				require.NoError(t, store.(session.RunStore).MoveRun(t.Context(), sess.ID, "librarian"))
				return sess
			},
		},
		{
			name: "the conversation had come back, on another agent's answer",
			left: func(t *testing.T, store session.Store) *session.Session {
				t.Helper()
				return leftBehind(t, store, "root", answerOf("librarian", "found it"))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := durableStore(t)
			sess := tt.left(t, store)
			prov := &transcriptProvider{}

			events := drain(durableRuntime(t, store, prov).ContinueStream(t.Context(), sess))

			assert.Empty(t, prov.received())
			assert.True(t, hasEventType(t, events, &ErrorEvent{}))
			assert.Empty(t, recordedRuns(t, store), "a run that is given up on is over")
		})
	}
}

// TestSubAgentCutOffWithItsRunStartsOver pins what a continued run knows of a
// sub-agent that was working when the run was cut off: only the root run is on
// record, so the delegating call comes back as interrupted and the work the
// sub-agent had done is not picked up.
func TestSubAgentCutOffWithItsRunStartsOver(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	delegating := func(rootModel, librarianModel *transcriptProvider) *LocalRuntime {
		librarian := agent.New("librarian", "Library agent", agent.WithModel(librarianModel))
		root := agent.New("root", "Root agent", agent.WithModel(rootModel),
			agent.WithToolSets(transfertask.New()), agent.WithSubAgents(librarian))
		rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, librarian)),
			WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)
		return rt
	}

	// The root delegates, and the run is suspended while the sub-agent answers.
	rootModel := &transcriptProvider{answer: func(context.Context) chat.MessageStream {
		return newStreamBuilder().
			AddToolCallName("call_1", "transfer_task").
			AddToolCallArguments("call_1", `{"agent":"librarian","task":"find a book","expected_output":"a title"}`).
			AddToolCallStopWithUsage(1, 1).
			Build()
	}}
	librarianModel := &transcriptProvider{answer: func(ctx context.Context) chat.MessageStream {
		return &cutOffStream{cancelled: ctx.Done()}
	}}
	sess := session.New(session.WithUserMessage("find a book"), session.WithToolsApproved(true))
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	for ev := range delegating(rootModel, librarianModel).RunStream(ctx, sess) {
		if answer, ok := ev.(*AgentChoiceEvent); ok && answer.AgentName == "librarian" {
			cancel(ErrSuspended)
		}
	}

	assert.Equal(t, []session.Run{{SessionID: sess.ID, Agent: "root", Current: "root", Suspended: true}}, recordedRuns(t, store), "the sub-agent's run is not on record")

	rootModel, librarianModel = &transcriptProvider{}, &transcriptProvider{}
	drain(delegating(rootModel, librarianModel).ContinueStream(t.Context(), reload(t, store, sess.ID)))

	requests := rootModel.received()
	require.Len(t, requests, 1)
	result := requests[0][len(requests[0])-1]
	assert.Equal(t, "call_1", result.ToolCallID)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content, "interrupted")
	assert.Empty(t, librarianModel.received(), "the sub-agent is not continued: its parent decides whether to call it again")
	assert.Empty(t, recordedRuns(t, store))
}
