package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
)

// transcriptProvider keeps the conversation of every request it receives and
// answers each with the stream answer returns, or with "done".
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

// durableRuntime is a durable runtime over store, as a process that has just
// started would build it.
func durableRuntime(t *testing.T, store session.Store, agents ...*agent.Agent) *DurableRuntime {
	t.Helper()
	rt, err := NewDurableRuntime(t.Context(), team.New(team.WithAgents(agents...)),
		WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	return rt
}

func rootAgent(model *transcriptProvider, opts ...agent.Opt) *agent.Agent {
	return agent.New("root", "You are a test agent", append([]agent.Opt{agent.WithModel(model)}, opts...)...)
}

// leftAt stores what a process that died leaves of a run of root: the record,
// standing at cp, and the transcript that record stands for, which starts
// with the prompt.
func leftAt(t *testing.T, store session.Store, cp checkpoint, transcript ...*session.Message) string {
	t.Helper()
	ctx := t.Context()
	sess := session.New(session.WithUserMessage("build it"))
	runs := store.(session.RunStore)
	require.NoError(t, runs.BeginRun(ctx, sess, nil, []byte(`{}`)))
	for _, msg := range transcript {
		_, err := store.AddMessage(ctx, sess.ID, msg)
		require.NoError(t, err)
	}
	cp.Version = checkpointVersion
	if cp.Agent.Current == "" {
		cp.Agent.Current = "root"
	}
	data, err := json.Marshal(cp)
	require.NoError(t, err)
	require.NoError(t, runs.SaveRun(ctx, sess.ID, data))
	return sess.ID
}

// continued takes up the run of a session and returns what its stream emitted.
func continued(t *testing.T, rt *DurableRuntime, sessionID string) []Event {
	t.Helper()
	_, events, err := rt.ContinueStream(t.Context(), sessionID, session.WithNonInteractive(true), session.WithToolsApproved(true))
	require.NoError(t, err)
	return drain(events)
}

func drain(events <-chan Event) []Event {
	var all []Event
	for ev := range events {
		all = append(all, ev)
	}
	return all
}

func recordedRuns(t *testing.T, store session.Store) []session.Run {
	t.Helper()
	runs, err := store.(session.RunStore).ListRuns(t.Context())
	require.NoError(t, err)
	return runs
}

// standsAt decodes where the only run on record stands.
func standsAt(t *testing.T, store session.Store) checkpoint {
	t.Helper()
	runs := recordedRuns(t, store)
	require.Len(t, runs, 1)
	var cp checkpoint
	require.NoError(t, json.Unmarshal(runs[0].Checkpoint, &cp))
	return cp
}

func transcriptOf[M any](messages []M, message func(M) chat.Message) []string {
	lines := make([]string, 0, len(messages))
	for _, m := range messages {
		msg := message(m)
		lines = append(lines, string(msg.Role)+": "+msg.Content)
	}
	return lines
}

// stored lists the conversation the store holds for a session.
func stored(t *testing.T, store session.Store, id string) []string {
	t.Helper()
	sess, err := store.GetSession(t.Context(), id)
	require.NoError(t, err)
	return transcriptOf(sess.GetAllMessages(), func(m session.Message) chat.Message { return m.Message })
}

// sent lists the conversation of a model request.
func sent(request []chat.Message) []string {
	return transcriptOf(request, func(m chat.Message) chat.Message { return m })
}

// rootSaid is a message root's model gave, as the run loop records it.
func rootSaid(content string, calls ...tools.ToolCall) *session.Message {
	return session.NewAgentMessage("root", &chat.Message{
		Role: chat.MessageRoleAssistant, Content: content, ToolCalls: calls, Model: "test/mock-model",
	})
}

func toolCall(id, name string) tools.ToolCall {
	return tools.ToolCall{ID: id, Type: "function", Function: tools.FunctionCall{Name: name, Arguments: "{}"}}
}

func TestDurableRunIsOnRecordBeforeItsToolsStart(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	// A session the store has never seen, given a conversation to start from.
	sess := session.New(session.WithUserMessage("plan it"))
	sess.AddMessage(rootSaid("planned"))
	sess.AddMessage(session.UserMessage("build it"))

	var atToolStart checkpoint
	var storedAtToolStart []string
	probe := &probeToolSet{onTools: func() {
		atToolStart = standsAt(t, store)
		storedAtToolStart = stored(t, store, sess.ID)
	}}
	rt := durableRuntime(t, store, rootAgent(&transcriptProvider{}, agent.WithToolSets(probe)))

	drain(rt.RunStream(t.Context(), sess))

	assert.Equal(t, phaseStarting, atToolStart.Phase)
	startsFrom := []string{"user: plan it", "assistant: planned", "user: build it"}
	assert.Equal(t, startsFrom, storedAtToolStart, "the run is on record with all it starts from")
	assert.Equal(t, append(startsFrom, "assistant: done"), stored(t, store, sess.ID), "the prompt is stored once")
	assert.Empty(t, recordedRuns(t, store), "a run that is over leaves no record")
}

// cutOffMidAnswer runs a session until its model is answering, then cancels
// the run with cause.
func cutOffMidAnswer(t *testing.T, store session.Store, cause error) *session.Session {
	t.Helper()
	model := &transcriptProvider{answer: func(ctx context.Context) chat.MessageStream {
		return &cutOffStream{cancelled: ctx.Done()}
	}}
	sess := session.New(session.WithUserMessage("build it"))
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	for ev := range durableRuntime(t, store, rootAgent(model)).RunStream(ctx, sess) {
		if _, answering := ev.(*AgentChoiceEvent); answering {
			cancel(cause)
		}
	}
	return sess
}

func TestDurableRunThatIsCancelledLeavesNoRecord(t *testing.T) {
	t.Parallel()
	store := durableStore(t)

	cutOffMidAnswer(t, store, nil)

	assert.Empty(t, recordedRuns(t, store))
}

func TestSuspendedRunIsTakenUpAtItsModelCall(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	sess := cutOffMidAnswer(t, store, ErrSuspended)

	runs := recordedRuns(t, store)
	require.Len(t, runs, 1)
	assert.True(t, runs[0].Suspended)
	assert.Equal(t, phaseModel, standsAt(t, store).Phase)

	model := &transcriptProvider{}
	loaded, events, err := durableRuntime(t, store, rootAgent(model)).ContinueStream(t.Context(), sess.ID)
	require.NoError(t, err)
	all := drain(events)

	requests := model.received()
	require.Len(t, requests, 1)
	assert.Equal(t, []string{"user: build it"}, sent(requests[0]), "the answer that was cut off is not sent back")
	assert.False(t, hasEventType(t, all, &UserMessageEvent{}), "the prompt was announced by the run that was cut off")
	assert.Equal(t, []string{"user: build it", "assistant: done"}, stored(t, store, sess.ID), "nor is it kept")
	assert.Equal(t, "done", loaded.GetLastAssistantMessageContent())
	assert.Empty(t, recordedRuns(t, store))
}

func TestInputTakenUpBeforeTheModelIsAskedIsOnRecord(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	model := &transcriptProvider{answer: func(ctx context.Context) chat.MessageStream {
		return &cutOffStream{cancelled: ctx.Done()}
	}}
	rt := durableRuntime(t, store, rootAgent(model))
	// The user adds to their request before the model is first asked.
	require.NoError(t, rt.Steer(t.Context(), QueuedMessage{Content: "and test it"}))
	sess := session.New(session.WithUserMessage("build it"))
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	for ev := range rt.RunStream(ctx, sess) {
		if _, answering := ev.(*AgentChoiceEvent); answering {
			cancel(ErrSuspended)
		}
	}

	model = &transcriptProvider{}
	continued(t, durableRuntime(t, store, rootAgent(model)), sess.ID)

	requests := model.received()
	require.Len(t, requests, 1)
	require.Len(t, requests[0], 2, "what the model was asked is what it is asked again")
	assert.Contains(t, requests[0][1].Content, "and test it")
}

func TestDiscardRunLeavesTheSessionAtItsLastFinishedStep(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	sess := cutOffMidAnswer(t, store, ErrSuspended)
	// Half an answer is in the store, past the checkpoint.
	require.Len(t, stored(t, store, sess.ID), 2)

	require.NoError(t, durableRuntime(t, store, rootAgent(&transcriptProvider{})).DiscardRun(t.Context(), sess.ID))

	assert.Empty(t, recordedRuns(t, store))
	assert.Equal(t, []string{"user: build it"}, stored(t, store, sess.ID))
}

func TestRunSuspendedDuringAToolCallIsTakenUpAtThatCall(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		annotations tools.ToolAnnotations
		wantRuns    int32
		wantResult  string
	}{
		{name: "which is not run again", wantRuns: 1, wantResult: toolexec.InterruptedResult},
		{name: "which runs again when its tool is idempotent", annotations: tools.ToolAnnotations{IdempotentHint: true}, wantRuns: 2, wantResult: "built"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := durableStore(t)
			var runs atomic.Int32
			running := make(chan struct{})
			build := tools.Tool{
				Name:        "build",
				Parameters:  map[string]any{},
				Annotations: tt.annotations,
				Handler: func(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
					if runs.Add(1) > 1 {
						return tools.ResultSuccess("built"), nil
					}
					close(running)
					<-ctx.Done()
					return nil, ctx.Err()
				},
			}
			model := &transcriptProvider{answer: func(context.Context) chat.MessageStream {
				return newStreamBuilder().AddToolCallName("call_1", "build").AddToolCallArguments("call_1", `{}`).AddToolCallStopWithUsage(1, 1).Build()
			}}
			sess := session.New(session.WithUserMessage("build it"), session.WithToolsApproved(true))

			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			for ev := range durableRuntime(t, store, rootAgent(model, agent.WithTools(build))).RunStream(ctx, sess) {
				if _, called := ev.(*ToolCallEvent); called {
					<-running
					cancel(ErrSuspended)
				}
			}

			at := standsAt(t, store)
			assert.Equal(t, phaseTools, at.Phase)
			require.Len(t, at.Calls, 1)
			assert.Equal(t, toolexec.CallStarted, at.Calls["call_1"].Progress)

			model = &transcriptProvider{}
			continued(t, durableRuntime(t, store, rootAgent(model, agent.WithTools(build))), sess.ID)

			assert.Equal(t, tt.wantRuns, runs.Load())
			requests := model.received()
			require.Len(t, requests, 1, "the model is asked once, with the call's result")
			assert.Equal(t, []string{"user: build it", "assistant: ", "tool: " + tt.wantResult}, sent(requests[0]))
			assert.Equal(t, []string{"user: build it", "assistant: ", "tool: " + tt.wantResult, "assistant: done"}, stored(t, store, sess.ID))
			assert.Empty(t, recordedRuns(t, store))
		})
	}
}

// inStepStore checks, every time a checkpoint is saved, that it and the
// stored conversation agree about which tool calls have their result.
type inStepStore struct {
	session.Store
	session.RunStore

	t          *testing.T
	mu         sync.Mutex
	outOfStep  []string
	checkpoint int
}

func (s *inStepStore) SaveRun(ctx context.Context, sessionID string, data []byte) error {
	var cp checkpoint
	require.NoError(s.t, json.Unmarshal(data, &cp))
	sess, err := s.GetSession(ctx, sessionID)
	require.NoError(s.t, err)
	stored := make(map[string]bool)
	for _, msg := range sess.GetAllMessages() {
		if msg.Message.Role == chat.MessageRoleTool {
			stored[msg.Message.ToolCallID] = true
		}
	}
	s.mu.Lock()
	s.checkpoint++
	if cp.Phase == phaseTools {
		for _, call := range cp.Answer.Calls {
			if noted := cp.Calls[call.ID].Progress >= toolexec.CallRecorded; noted != stored[call.ID] {
				s.outOfStep = append(s.outOfStep, fmt.Sprintf("%s: noted=%t stored=%t", call.ID, noted, stored[call.ID]))
			}
		}
	}
	s.mu.Unlock()
	return s.RunStore.SaveRun(ctx, sessionID, data)
}

func TestParallelToolCallsAndTheirResultsAreRecordedInStep(t *testing.T) {
	t.Parallel()
	const calls = 12
	sqlite := durableStore(t)
	store := &inStepStore{Store: sqlite, RunStore: sqlite.(session.RunStore), t: t}
	work := tools.Tool{
		Name:       "work",
		Parameters: map[string]any{},
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("worked"), nil
		},
	}
	var asked atomic.Int32
	model := &transcriptProvider{answer: func(context.Context) chat.MessageStream {
		if asked.Add(1) > 1 {
			return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build()
		}
		batch := newStreamBuilder()
		for i := range calls {
			id := fmt.Sprintf("call_%d", i)
			batch.AddToolCallName(id, "work").AddToolCallArguments(id, `{}`)
		}
		return batch.AddToolCallStopWithUsage(1, 1).Build()
	}}
	sess := session.New(session.WithUserMessage("work"), session.WithToolsApproved(true))

	events := drain(durableRuntime(t, store, rootAgent(model, agent.WithTools(work))).RunStream(t.Context(), sess))

	require.False(t, hasEventType(t, events, &ErrorEvent{}))
	assert.Greater(t, store.checkpoint, calls, "every call is recorded as it goes")
	assert.Empty(t, store.outOfStep, "a run taken up from any of these checkpoints finds each result once")
}

func TestContinueStreamLeavesAFinishedToolCallAlone(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	var runs atomic.Int32
	build := tools.Tool{
		Name:       "build",
		Parameters: map[string]any{},
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			runs.Add(1)
			return tools.ResultSuccess("built again"), nil
		},
	}
	call := toolCall("call_1", "build")
	// The process died after the tool returned, before the model was asked.
	sessionID := leftAt(t, store,
		checkpoint{
			Phase:  phaseTools,
			Answer: &modelAnswer{Calls: []tools.ToolCall{call}, Model: modelsdev.ParseIDOrZero("test/mock-model")},
			Calls:  map[string]toolexec.CallRecord{"call_1": {Progress: toolexec.CallFinished}},
		},
		rootSaid("", call),
		session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleTool, ToolCallID: "call_1", Content: "built"}),
	)
	model := &transcriptProvider{}

	continued(t, durableRuntime(t, store, rootAgent(model, agent.WithTools(build))), sessionID)

	assert.Zero(t, runs.Load())
	require.Len(t, model.received(), 1)
	assert.Equal(t, []string{"user: build it", "assistant: ", "tool: built"}, sent(model.received()[0]))
}

func TestContinueStreamTakesUpAnAnswerAtWhatFollowsIt(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	// The process died after root answered, before its forced handoff.
	sessionID := leftAt(t, store,
		checkpoint{
			Phase:  phaseAnswered,
			Answer: &modelAnswer{Content: "extracted", Stopped: true, Model: modelsdev.ParseIDOrZero("test/mock-model")},
		},
		rootSaid("extracted"),
	)
	rootModel, summarizerModel := &transcriptProvider{}, &transcriptProvider{}
	summarizer := agent.New("summarizer", "You summarize", agent.WithModel(summarizerModel))

	continued(t, durableRuntime(t, store, rootAgent(rootModel, agent.WithForceHandoff(summarizer)), summarizer), sessionID)

	assert.Empty(t, rootModel.received(), "the agent that had answered is not asked again")
	require.Len(t, summarizerModel.received(), 1, "the handoff its answer was to trigger still happens")
	transcript := stored(t, store, sessionID)
	require.Len(t, transcript, 4)
	assert.Equal(t, "assistant: extracted", transcript[1])
	assert.Equal(t, "assistant: done", transcript[3])
	assert.Empty(t, recordedRuns(t, store))
}

func TestContinueStreamPutsTheConversationBackWithItsAgent(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	call := toolCall("call_1", "handoff")
	// The process died after root handed the conversation to worker.
	sessionID := leftAt(t, store,
		checkpoint{Phase: phaseModel, Agent: agentState{Current: "worker"}},
		rootSaid("", call),
		session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleTool, ToolCallID: "call_1", Content: "handed off"}),
	)
	rootModel, workerModel := &transcriptProvider{}, &transcriptProvider{}
	worker := agent.New("worker", "You work", agent.WithModel(workerModel))
	rt := durableRuntime(t, store, rootAgent(rootModel), worker)

	// Where the run stands is its record's to say, not its host's.
	_, events, err := rt.ContinueStream(t.Context(), sessionID, session.WithNonInteractive(true), session.WithAgentName("root"))
	require.NoError(t, err)
	drain(events)

	assert.Empty(t, rootModel.received())
	assert.Len(t, workerModel.received(), 1)
	assert.Equal(t, "worker", rt.CurrentAgentName(t.Context()))
}

func TestContinueStreamRestoresWhatTheRunCarried(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	// The run had used up its iterations when the process died, and its
	// user had let it run a tool without asking, which the store was never
	// told.
	sessionID := leftAt(t, store, checkpoint{
		Phase:   phaseModel,
		State:   runState{Iteration: 3, MaxIterations: 3},
		Session: sessionState{Permissions: &session.PermissionsConfig{Allow: []string{"shell"}}},
	})
	model := &transcriptProvider{}

	events := continued(t, durableRuntime(t, store, rootAgent(model)), sessionID)

	assert.Empty(t, model.received(), "the turns the run had taken still count")
	assert.True(t, hasEventType(t, events, &MaxIterationsReachedEvent{}))
	sess, err := store.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, []string{"shell"}, sess.ClonePermissions().Allow, "the run has what its user had let it do")
}

func TestSubAgentCutOffWithItsRunStartsOver(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	delegating := func(rootModel, librarianModel *transcriptProvider) *DurableRuntime {
		librarian := agent.New("librarian", "Library agent", agent.WithModel(librarianModel))
		return durableRuntime(t, store,
			rootAgent(rootModel, agent.WithToolSets(transfertask.New()), agent.WithSubAgents(librarian)), librarian)
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

	runs := recordedRuns(t, store)
	require.Len(t, runs, 1, "the sub-agent's run is not on record")
	assert.Equal(t, sess.ID, runs[0].SessionID)

	rootModel, librarianModel = &transcriptProvider{}, &transcriptProvider{}
	continued(t, delegating(rootModel, librarianModel), sess.ID)

	requests := rootModel.received()
	require.Len(t, requests, 1)
	result := requests[0][len(requests[0])-1]
	assert.Equal(t, "call_1", result.ToolCallID)
	assert.Equal(t, toolexec.InterruptedResult, result.Content)
	assert.Empty(t, librarianModel.received(), "the sub-agent is not taken up: its parent decides whether to call it again")
	assert.Empty(t, recordedRuns(t, store))
}

func TestContinueStreamCountsACrashUntilTheRunGetsPastTheStep(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	sessionID := leftAt(t, store, checkpoint{Phase: phaseModel})
	crashes := -1
	model := &transcriptProvider{answer: func(context.Context) chat.MessageStream {
		crashes = recordedRuns(t, store)[0].Crashes
		return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build()
	}}

	continued(t, durableRuntime(t, store, rootAgent(model)), sessionID)

	assert.Equal(t, 1, crashes, "taking a run up is not progress: the crash counts while the step is redone")
}

func TestContinueStreamGivesUpARunThatKeepsDying(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	sessionID := leftAt(t, store, checkpoint{Phase: phaseModel, Session: sessionState{InputTokens: 9000, OutputTokens: 400}})
	// Every earlier attempt to take the run up died at the same step, and
	// the last one left the session ahead of its conversation.
	for range maxRunCrashes - 1 {
		_, err := store.(session.RunStore).ResumeRun(t.Context(), sessionID)
		require.NoError(t, err)
	}
	require.NoError(t, store.UpdateSessionTokens(t.Context(), sessionID, 10, 5, 0))
	model := &transcriptProvider{}

	_, _, err := durableRuntime(t, store, rootAgent(model)).ContinueStream(t.Context(), sessionID)

	require.Error(t, err)
	assert.Empty(t, model.received())
	assert.Empty(t, recordedRuns(t, store), "a run that is given up on is over")
	left, err := store.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	input, output := left.Usage()
	assert.EqualValues(t, 9000, input, "the session is left as it stood at the run's last finished step")
	assert.EqualValues(t, 400, output)
}

func TestContinueStreamOfASessionWithoutARun(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	sess := session.New()
	require.NoError(t, store.AddSession(t.Context(), sess))

	_, _, err := durableRuntime(t, store, rootAgent(&transcriptProvider{})).ContinueStream(t.Context(), sess.ID)

	require.ErrorIs(t, err, session.ErrNotFound)
}

// failingStore is a session store that fails the writes it is told to.
type failingStore struct {
	session.Store
	session.RunStore

	failSaveRun, failAddMessage, failEndRun bool
}

var errDiskFull = errors.New("disk full")

func (s *failingStore) SaveRun(ctx context.Context, sessionID string, checkpoint []byte) error {
	if s.failSaveRun {
		return errDiskFull
	}
	return s.RunStore.SaveRun(ctx, sessionID, checkpoint)
}

func (s *failingStore) EndRun(ctx context.Context, sessionID string) error {
	if s.failEndRun {
		return errDiskFull
	}
	return s.RunStore.EndRun(ctx, sessionID)
}

func (s *failingStore) AddMessage(ctx context.Context, sessionID string, msg *session.Message) (int64, error) {
	if s.failAddMessage && msg.Message.Role == chat.MessageRoleAssistant {
		return 0, errDiskFull
	}
	return s.Store.AddMessage(ctx, sessionID, msg)
}

func TestDurableRunStopsWhenItCannotBeRecorded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		fail      func(*failingStore)
		wantCalls int
		wantPhase runPhase
	}{
		{
			name:      "its checkpoint cannot be saved",
			fail:      func(s *failingStore) { s.failSaveRun = true },
			wantPhase: phaseStarting,
		},
		{
			name:      "the store misses part of the conversation",
			fail:      func(s *failingStore) { s.failAddMessage = true },
			wantCalls: 1,
			wantPhase: phaseModel,
		},
		{
			name:      "its end cannot be recorded",
			fail:      func(s *failingStore) { s.failEndRun = true },
			wantCalls: 1,
			wantPhase: phaseAnswered,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sqlite := durableStore(t)
			store := &failingStore{Store: sqlite, RunStore: sqlite.(session.RunStore)}
			tt.fail(store)
			model := &transcriptProvider{}
			sess := session.New(session.WithUserMessage("build it"))

			events := drain(durableRuntime(t, store, rootAgent(model)).RunStream(t.Context(), sess))

			assert.Len(t, model.received(), tt.wantCalls)
			assert.True(t, hasEventType(t, events, &ErrorEvent{}))
			stopped, ok := events[len(events)-1].(*StreamStoppedEvent)
			require.True(t, ok)
			assert.Equal(t, turnEndReasonError, stopped.Reason)
			assert.Equal(t, tt.wantPhase, standsAt(t, sqlite).Phase, "the record stays where the run last stood")
		})
	}
}

func TestDurableRuntimeDoesNotRunAnAgentInAHarness(t *testing.T) {
	t.Parallel()
	store := durableStore(t)
	model := &transcriptProvider{}
	harnessed := rootAgent(model, agent.WithHarness(&latest.HarnessConfig{Type: "claude-code"}))
	sess := session.New(session.WithUserMessage("build it"))

	events := drain(durableRuntime(t, store, harnessed).RunStream(t.Context(), sess))

	assert.True(t, hasEventType(t, events, &ErrorEvent{}))
	assert.Empty(t, model.received())
	assert.Empty(t, recordedRuns(t, store))
}

func TestNewDurableRuntimeNeedsAStoreThatRecordsRuns(t *testing.T) {
	t.Parallel()

	_, err := NewDurableRuntime(t.Context(), team.New(team.WithAgents(rootAgent(&transcriptProvider{}))),
		WithSessionStore(session.NewInMemorySessionStore()), WithModelStore(mockModelStore{}))

	require.Error(t, err)
}
