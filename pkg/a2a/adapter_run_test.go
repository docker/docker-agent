package a2a

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/adk/v2/agent"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	dagent "github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/servesafety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// mockStream replays a fixed sequence of completion chunks.
type mockStream struct {
	responses []chat.MessageStreamResponse
	idx       int
}

func (m *mockStream) Recv() (chat.MessageStreamResponse, error) {
	if m.idx >= len(m.responses) {
		return chat.MessageStreamResponse{}, io.EOF
	}
	resp := m.responses[m.idx]
	m.idx++
	return resp, nil
}

func (m *mockStream) Close() {}

// mockProvider returns a predetermined stream, or err when set.
type mockProvider struct {
	id     modelsdev.ID
	stream chat.MessageStream
	err    error
}

func (m *mockProvider) ID() modelsdev.ID { return m.id }

func (m *mockProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.stream, nil
}

func (m *mockProvider) BaseConfig() base.Config { return base.Config{} }

func (m *mockProvider) MaxTokens() int { return 0 }

// newMockTeam builds a single-agent team whose model streams the given
// content chunks followed by a terminal stop.
func newMockTeam(chunks ...string) (*team.Team, *dagent.Agent) {
	responses := make([]chat.MessageStreamResponse, 0, len(chunks)+1)
	for _, chunk := range chunks {
		responses = append(responses, chat.MessageStreamResponse{
			Choices: []chat.MessageStreamChoice{{
				Index: 0,
				Delta: chat.MessageDelta{Content: chunk},
			}},
		})
	}
	responses = append(responses, chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{
			Index:        0,
			FinishReason: chat.FinishReasonStop,
		}},
		Usage: &chat.Usage{InputTokens: 3, OutputTokens: 7},
	})
	prov := &mockProvider{
		id:     modelsdev.NewID("test", "mock-model"),
		stream: &mockStream{responses: responses},
	}
	return newTeamWithProvider(prov)
}

func newTeamWithProvider(prov provider.Provider) (*team.Team, *dagent.Agent) {
	root := dagent.New("root", "You are a test agent", dagent.WithModel(prov))
	return team.New(team.WithAgents(root)), root
}

// fakeADKSession implements the ADK session interface; only ID matters
// to runDockerAgent.
type fakeADKSession struct{ id string }

func (s fakeADKSession) ID() string                { return s.id }
func (s fakeADKSession) AppName() string           { return "test-app" }
func (s fakeADKSession) UserID() string            { return "test-user" }
func (s fakeADKSession) State() adksession.State   { return nil }
func (s fakeADKSession) Events() adksession.Events { return nil }
func (s fakeADKSession) LastUpdateTime() time.Time { return time.Time{} }

// fakeInvocationContext implements agent.InvocationContext with the minimal
// behavior runDockerAgent relies on: the embedded context, Session().ID(),
// UserContent(), and Ended().
type fakeInvocationContext struct {
	context.Context //nolint:containedctx // agent.InvocationContext embeds context.Context

	sess        adksession.Session
	userContent *genai.Content
	ended       *atomic.Bool
}

func newFakeInvocationContext(ctx context.Context, sessionID, userMessage string) *fakeInvocationContext {
	return &fakeInvocationContext{
		Context:     ctx,
		sess:        fakeADKSession{id: sessionID},
		userContent: genai.NewContentFromText(userMessage, genai.RoleUser),
		ended:       &atomic.Bool{},
	}
}

func (c *fakeInvocationContext) Agent() agent.Agent              { return nil }
func (c *fakeInvocationContext) Artifacts() agent.Artifacts      { return nil }
func (c *fakeInvocationContext) Memory() agent.Memory            { return nil }
func (c *fakeInvocationContext) Session() adksession.Session     { return c.sess }
func (c *fakeInvocationContext) InvocationID() string            { return "test-invocation" }
func (c *fakeInvocationContext) Branch() string                  { return "" }
func (c *fakeInvocationContext) IsolationScope() string          { return "" }
func (c *fakeInvocationContext) UserContent() *genai.Content     { return c.userContent }
func (c *fakeInvocationContext) RunConfig() *agent.RunConfig     { return nil }
func (c *fakeInvocationContext) EndInvocation()                  { c.ended.Store(true) }
func (c *fakeInvocationContext) Ended() bool                     { return c.ended.Load() }
func (c *fakeInvocationContext) ResumedInput(string) (any, bool) { return nil, false }

func (c *fakeInvocationContext) WithContext(ctx context.Context) agent.InvocationContext {
	return &fakeInvocationContext{
		Context:     ctx,
		sess:        c.sess,
		userContent: c.userContent,
		ended:       c.ended,
	}
}

func (c *fakeInvocationContext) WithICDelta(*agent.InvocationContextDelta) agent.InvocationContext {
	return c
}

// recordingStore captures the live *session.Session pointers the runtime
// persists via UpdateSession (fired on run start), so tests can inspect the
// session runDockerAgent built or resumed, including fields the in-memory
// store does not persist (e.g. NonInteractive).
type recordingStore struct {
	session.Store

	mu      sync.Mutex
	updated []*session.Session
}

func newRecordingStore() *recordingStore {
	return &recordingStore{Store: session.NewInMemorySessionStore()}
}

func (s *recordingStore) UpdateSession(ctx context.Context, sess *session.Session) error {
	s.mu.Lock()
	s.updated = append(s.updated, sess)
	s.mu.Unlock()
	return s.Store.UpdateSession(ctx, sess)
}

func (s *recordingStore) updatedSessions() []*session.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.updated)
}

const testWorkspaceRoot = "/srv/a2a-workspace"

type yieldedEvent struct {
	event *adksession.Event
	err   error
}

func collectRunEvents(ctx agent.InvocationContext, tm *team.Team, a *dagent.Agent, store session.Store, policy session.SafetyPolicy) []yieldedEvent {
	var out []yieldedEvent
	for ev, err := range runDockerAgent(ctx, tm, a.Name(), a, store, servesafety.Resolved{Policy: policy}, testWorkspaceRoot) {
		out = append(out, yieldedEvent{event: ev, err: err})
	}
	return out
}

func eventText(t *testing.T, ev *adksession.Event) string {
	t.Helper()

	require.NotNil(t, ev)
	require.NotNil(t, ev.Content)
	require.Len(t, ev.Content.Parts, 1)
	return ev.Content.Parts[0].Text
}

func TestRunDockerAgent_StreamsPartialAndFinalEvents(t *testing.T) {
	t.Parallel()

	tm, root := newMockTeam("Hello, ", "world!")
	store := session.NewInMemorySessionStore()
	ctx := newFakeInvocationContext(t.Context(), "a2a-ctx-events", "Hi there")

	events := collectRunEvents(ctx, tm, root, store, session.SafetyPolicyRestricted)

	require.Len(t, events, 3)
	for _, e := range events {
		require.NoError(t, e.err)
	}

	first := events[0].event
	assert.Equal(t, "root", first.Author)
	assert.True(t, first.Partial)
	assert.False(t, first.TurnComplete)
	assert.Equal(t, genai.RoleModel, first.Content.Role)
	assert.Equal(t, "Hello, ", eventText(t, first))

	second := events[1].event
	assert.True(t, second.Partial)
	assert.Equal(t, "world!", eventText(t, second))

	final := events[2].event
	assert.Equal(t, "root", final.Author)
	assert.False(t, final.Partial)
	assert.True(t, final.TurnComplete)
	assert.Equal(t, genai.FinishReasonStop, final.FinishReason)
	assert.Equal(t, genai.RoleModel, final.Content.Role)
	assert.Equal(t, "Hello, world!", eventText(t, final))
}

func TestRunDockerAgent_ErrorEventStopsIteration(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{
		id:  modelsdev.NewID("test", "mock-model"),
		err: errors.New("simulated stream failure"),
	}
	tm, root := newTeamWithProvider(prov)
	store := session.NewInMemorySessionStore()
	ctx := newFakeInvocationContext(t.Context(), "a2a-ctx-error", "Hi")

	events := collectRunEvents(ctx, tm, root, store, session.SafetyPolicyRestricted)

	require.Len(t, events, 1)
	assert.Nil(t, events[0].event)
	require.Error(t, events[0].err)
	assert.Contains(t, events[0].err.Error(), "simulated stream failure")
}

// An empty stream (stop without content) currently produces no final event:
// the StreamStopped branch only yields when content was accumulated.
func TestRunDockerAgent_EmptyStreamEmitsNoFinalEvent(t *testing.T) {
	t.Parallel()

	tm, root := newMockTeam()
	store := session.NewInMemorySessionStore()
	ctx := newFakeInvocationContext(t.Context(), "a2a-ctx-empty", "Hi")

	events := collectRunEvents(ctx, tm, root, store, session.SafetyPolicyRestricted)

	assert.Empty(t, events)
}

func TestRunDockerAgent_ConsumerStopsEarly(t *testing.T) {
	t.Parallel()

	tm, root := newMockTeam("first", "second")
	store := session.NewInMemorySessionStore()
	ctx := newFakeInvocationContext(t.Context(), "a2a-ctx-early-stop", "Hi")

	var events []*adksession.Event
	for ev, err := range runDockerAgent(ctx, tm, root.Name(), root, store, servesafety.Resolved{Policy: session.SafetyPolicyRestricted}, testWorkspaceRoot) {
		require.NoError(t, err)
		events = append(events, ev)
		break
	}

	require.Len(t, events, 1)
	assert.True(t, events[0].Partial)
	assert.Equal(t, "first", eventText(t, events[0]))
}

func TestRunDockerAgent_EndedInvocationStopsIteration(t *testing.T) {
	t.Parallel()

	tm, root := newMockTeam("first", "second")
	store := session.NewInMemorySessionStore()
	ctx := newFakeInvocationContext(t.Context(), "a2a-ctx-ended", "Hi")

	var events []*adksession.Event
	for ev, err := range runDockerAgent(ctx, tm, root.Name(), root, store, servesafety.Resolved{Policy: session.SafetyPolicyRestricted}, testWorkspaceRoot) {
		require.NoError(t, err)
		events = append(events, ev)
		// Ending the invocation after the first chunk must stop the
		// adapter before it yields the second chunk or a final event.
		ctx.EndInvocation()
	}

	require.Len(t, events, 1)
	assert.Equal(t, "first", eventText(t, events[0]))
}

func TestRunDockerAgent_NewSessionUsesA2ASettings(t *testing.T) {
	t.Parallel()

	tm, root := newMockTeam("answer")
	store := newRecordingStore()
	ctx := newFakeInvocationContext(t.Context(), "a2a-ctx-new", "What is Docker?")

	events := collectRunEvents(ctx, tm, root, store, session.SafetyPolicyRestricted)
	require.Len(t, events, 2)

	updated := store.updatedSessions()
	require.NotEmpty(t, updated)
	sess := updated[0]

	assert.Equal(t, "a2a-ctx-new", sess.ID)
	assert.Equal(t, "a2a", sess.Origin)
	assert.Equal(t, "A2A Session a2a-ctx-new", sess.Title)
	assert.Equal(t, session.SafetyPolicyRestricted, sess.GetSafetyPolicy())
	assert.False(t, sess.ToolsApproved)
	assert.True(t, sess.NonInteractive)

	// runDockerAgent receives the server workspace at startup, so tests use a
	// fixed value rather than reading the process working directory.
	assert.Equal(t, testWorkspaceRoot, sess.WorkingDir)

	msgs := sess.GetAllMessages()
	require.NotEmpty(t, msgs)
	assert.Equal(t, chat.MessageRoleUser, msgs[0].Message.Role)
	assert.Equal(t, "What is Docker?", msgs[0].Message.Content)

	stored, err := store.GetSession(t.Context(), "a2a-ctx-new")
	require.NoError(t, err)
	assert.Equal(t, "a2a-ctx-new", stored.ID)
	assert.Equal(t, "a2a", stored.Origin)
	assert.Equal(t, "A2A Session a2a-ctx-new", stored.Title)
}

func TestRunDockerAgent_RejectsNonA2ASessionCollision(t *testing.T) {
	t.Parallel()

	for _, origin := range []string{"run", "", "acp"} {
		t.Run(origin, func(t *testing.T) {
			tm, root := newMockTeam("answer")
			store := newRecordingStore()
			existing := session.New(
				session.WithID("a2a-ctx-collision"),
				session.WithOrigin(origin),
				session.WithTitle("Private Session"),
				session.WithUserMessage("private history"),
			)
			require.NoError(t, store.AddSession(t.Context(), existing))

			ctx := newFakeInvocationContext(t.Context(), "a2a-ctx-collision", "A2A request")
			for range 2 {
				events := collectRunEvents(ctx, tm, root, store, session.SafetyPolicyRestricted)
				require.Len(t, events, 1)
				require.ErrorContains(t, events[0].err, "context ID is not available")
			}

			stored, err := store.GetSession(t.Context(), "a2a-ctx-collision")
			require.NoError(t, err)
			assert.Equal(t, origin, stored.Origin)
			assert.Equal(t, "Private Session", stored.Title)
			assert.Len(t, stored.GetAllMessages(), 1)
			assert.Empty(t, store.updatedSessions())
		})
	}
}

func TestRunDockerAgent_ExplicitSafety(t *testing.T) {
	t.Parallel()

	for _, policy := range []session.SafetyPolicy{
		session.SafetyPolicyStrict,
		session.SafetyPolicyBalanced,
		session.SafetyPolicyRestricted,
		session.SafetyPolicyAutonomous,
	} {
		t.Run(string(policy), func(t *testing.T) {
			t.Parallel()

			tm, root := newMockTeam("answer")
			store := newRecordingStore()
			ctx := newFakeInvocationContext(t.Context(), "a2a-ctx-"+string(policy), "What is Docker?")

			collectRunEvents(ctx, tm, root, store, policy)

			updated := store.updatedSessions()
			require.NotEmpty(t, updated)
			assert.Equal(t, policy, updated[0].GetSafetyPolicy())
			assert.Equal(t, policy == session.SafetyPolicyAutonomous, updated[0].ToolsApproved)
		})
	}
}

func TestRunDockerAgent_ResumedSessionDoesNotExceedServerSafety(t *testing.T) {
	t.Parallel()

	tm, root := newMockTeam("answer")
	store := newRecordingStore()
	existing := session.New(
		session.WithID("a2a-ctx-ceiling"),
		session.WithOrigin("a2a"),
		session.WithSafetyPolicy(session.SafetyPolicyAutonomous),
	)
	require.NoError(t, store.AddSession(t.Context(), existing))

	ctx := newFakeInvocationContext(t.Context(), "a2a-ctx-ceiling", "follow-up question")
	collectRunEvents(ctx, tm, root, store, session.SafetyPolicyBalanced)

	assert.Equal(t, session.SafetyPolicyBalanced, existing.GetSafetyPolicy())
	assert.False(t, existing.ToolsApproved)
	assert.True(t, existing.NonInteractive)
}

func TestRunDockerAgent_ResumedSaferSessionIsPreserved(t *testing.T) {
	t.Parallel()

	tm, root := newMockTeam("answer")
	store := newRecordingStore()
	existing := session.New(
		session.WithID("a2a-ctx-preserve"),
		session.WithOrigin("a2a"),
		session.WithSafetyPolicy(session.SafetyPolicyStrict),
	)
	require.NoError(t, store.AddSession(t.Context(), existing))

	ctx := newFakeInvocationContext(t.Context(), "a2a-ctx-preserve", "follow-up question")
	collectRunEvents(ctx, tm, root, store, session.SafetyPolicyAutonomous)

	assert.Equal(t, session.SafetyPolicyStrict, existing.GetSafetyPolicy())
	assert.False(t, existing.ToolsApproved)
	assert.True(t, existing.NonInteractive)
}

func TestRunDockerAgent_ResumesExistingSession(t *testing.T) {
	t.Parallel()

	tm, root := newMockTeam("resumed answer")
	store := newRecordingStore()

	existing := session.New(
		session.WithID("a2a-ctx-resume"),
		session.WithOrigin("a2a"),
		session.WithTitle("Existing Title"),
	)
	require.NoError(t, store.AddSession(t.Context(), existing))

	ctx := newFakeInvocationContext(t.Context(), "a2a-ctx-resume", "follow-up question")

	events := collectRunEvents(ctx, tm, root, store, session.SafetyPolicyRestricted)
	require.Len(t, events, 2)

	updated := store.updatedSessions()
	require.NotEmpty(t, updated)
	assert.Same(t, existing, updated[0], "the stored session should be resumed, not recreated")

	assert.Equal(t, "Existing Title", existing.Title)
	assert.Equal(t, session.SafetyPolicyRestricted, existing.GetSafetyPolicy())
	assert.False(t, existing.ToolsApproved)
	assert.True(t, existing.NonInteractive)

	msgs := existing.GetAllMessages()
	require.NotEmpty(t, msgs)
	assert.Equal(t, chat.MessageRoleUser, msgs[0].Message.Role)
	assert.Equal(t, "follow-up question", msgs[0].Message.Content)

	final := events[1].event
	assert.True(t, final.TurnComplete)
	assert.Equal(t, "resumed answer", eventText(t, final))
}

func TestRunDockerAgent_RuntimeCreationError(t *testing.T) {
	t.Parallel()

	// The agent is deliberately not part of the team: runDockerAgent only
	// reads session limits from the agent argument before building the
	// runtime, while runtime.New consults the team alone — and fails here
	// because the empty team has no default agent.
	root := dagent.New("root", "You are a test agent")
	emptyTeam := team.New()
	store := session.NewInMemorySessionStore()
	ctx := newFakeInvocationContext(t.Context(), "a2a-ctx-no-team", "Hi")

	events := collectRunEvents(ctx, emptyTeam, root, store, session.SafetyPolicyRestricted)

	require.Len(t, events, 1)
	assert.Nil(t, events[0].event)
	require.Error(t, events[0].err)
	assert.Contains(t, events[0].err.Error(), "failed to create runtime")
}
