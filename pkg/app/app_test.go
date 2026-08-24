package app

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// mockRuntime is a minimal mock for testing App without a real runtime.
// Snapshot operations are NOT modeled here: they are driven through a
// [builtins.SnapshotController] passed to the App via WithSnapshotController,
// so the mock runtime stays small and focused on the runtime.Runtime
// surface.
type mockRuntime struct {
	store session.Store
}

func (m *mockRuntime) CurrentAgentInfo(ctx context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{}
}
func (m *mockRuntime) CurrentAgentName(context.Context) string              { return "mock" }
func (m *mockRuntime) SetCurrentAgent(_ context.Context, name string) error { return nil }
func (m *mockRuntime) CurrentAgentTools(ctx context.Context) ([]tools.Tool, error) {
	return nil, nil
}

func (m *mockRuntime) CurrentAgentToolsetStatuses() []tools.ToolsetStatus { return nil }
func (m *mockRuntime) RestartToolset(context.Context, string) error       { return nil }

func (m *mockRuntime) EmitStartupInfo(ctx context.Context, sess *session.Session, events runtime.EventSink) {
}
func (m *mockRuntime) EmitAgentInfo(context.Context, runtime.EventSink) {}
func (m *mockRuntime) ResetStartupInfo()                                {}
func (m *mockRuntime) RunStream(ctx context.Context, sess *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event)
	close(ch)
	return ch
}

func (m *mockRuntime) Run(ctx context.Context, sess *session.Session) ([]session.Message, error) {
	return nil, nil
}
func (m *mockRuntime) Resume(ctx context.Context, req runtime.ResumeRequest) {}
func (m *mockRuntime) ResumeElicitation(ctx context.Context, action tools.ElicitationAction, content map[string]any, elicitationID ...string) error {
	return nil
}
func (m *mockRuntime) SessionStore() session.Store { return m.store }
func (m *mockRuntime) Summarize(ctx context.Context, sess *session.Session, additionalPrompt string, events runtime.EventSink) {
}
func (m *mockRuntime) PermissionsInfo() *runtime.PermissionsInfo { return nil }
func (m *mockRuntime) CurrentAgentSkillsToolset() *skillstool.ToolSet {
	return nil
}

func (m *mockRuntime) RunSkillFork(context.Context, *session.Session, skillstool.RunSkillArgs, runtime.EventSink) (*tools.ToolCallResult, error) {
	return nil, nil
}

func (m *mockRuntime) CurrentMCPPrompts(context.Context) map[string]mcptools.PromptInfo {
	return make(map[string]mcptools.PromptInfo)
}

func (m *mockRuntime) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", nil
}

func (m *mockRuntime) UpdateSessionTitle(_ context.Context, sess *session.Session, title string) error {
	sess.Title = title
	return nil
}
func (m *mockRuntime) TitleGenerator(context.Context) *sessiontitle.Generator    { return nil }
func (m *mockRuntime) Close() error                                              { return nil }
func (m *mockRuntime) Stop()                                                     {}
func (m *mockRuntime) Steer(_ context.Context, _ runtime.QueuedMessage) error    { return nil }
func (m *mockRuntime) FollowUp(_ context.Context, _ runtime.QueuedMessage) error { return nil }
func (m *mockRuntime) QueueStatus() runtime.QueueStatus                          { return runtime.QueueStatus{} }

func (m *mockRuntime) TogglePause(context.Context) (bool, error) { return false, nil }

func (m *mockRuntime) SetAgentModel(context.Context, string, string) error {
	return nil
}

func (m *mockRuntime) CycleAgentThinkingLevel(context.Context, string) (effort.Level, error) {
	return "", runtime.ErrUnsupported
}

func (m *mockRuntime) SetAgentThinkingLevel(context.Context, string, effort.Level) (effort.Level, error) {
	return "", runtime.ErrUnsupported
}
func (m *mockRuntime) AvailableModels(context.Context) []runtime.ModelChoice { return nil }
func (m *mockRuntime) SupportsModelSwitching() bool                          { return false }
func (m *mockRuntime) OnToolsChanged(func(runtime.Event))                    {}
func (m *mockRuntime) OnBackgroundEvent(func(runtime.Event))                 {}
func (m *mockRuntime) OnElicitationRequest(func(runtime.Event))              {}

// Verify mockRuntime implements runtime.Runtime
var _ runtime.Runtime = (*mockRuntime)(nil)

// retryMockRuntime mimics the real run loop's startup event ordering: it
// re-emits a UserMessageEvent for the session's trailing message BEFORE
// StreamStarted (exactly what LocalRuntime.runStreamLoop does when
// SendUserMessage is set), then a StreamStopped. Used to verify App.Retry
// suppresses the pre-StreamStarted re-emission.
type retryMockRuntime struct {
	mockRuntime
}

func (m *retryMockRuntime) RunStream(_ context.Context, sess *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event, 8)
	go func() {
		defer close(ch)
		// Re-emitted user message (pre-StreamStarted): must be suppressed.
		ch <- runtime.UserMessage("hello", sess.ID, nil, 0)
		ch <- runtime.StreamStarted(sess.ID, "mock")
		// A genuine mid-run user message (post-StreamStarted): must pass through.
		ch <- runtime.UserMessage("steered", sess.ID, nil, 1)
		ch <- runtime.StreamStopped(sess.ID, "mock", "normal")
	}()
	return ch
}

// blockingRunStreamRuntime's RunStream blocks until release is closed (or
// ctx is cancelled), letting tests observe a streamGuard held for the
// stream's actual duration instead of racing a fake runtime that returns
// immediately.
type blockingRunStreamRuntime struct {
	mockRuntime

	release chan struct{}
}

func (r *blockingRunStreamRuntime) RunStream(ctx context.Context, _ *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event)
	go func() {
		defer close(ch)
		select {
		case <-r.release:
		case <-ctx.Done():
		}
	}()
	return ch
}

// TestApp_Run_HoldsStreamGuardForStreamDuration pins the #3590 fix: Run must
// hold the WithStreamGuard lock for the entire direct RunStream call, not
// just while scheduling it, so a concurrent SessionManager.AddMessage/
// UpdateMessage/RunSession sharing the same lock (see AttachRuntime) sees
// the attached stream as busy for as long as it is genuinely active.
func TestApp_Run_HoldsStreamGuardForStreamDuration(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	rt := &blockingRunStreamRuntime{release: release}
	var guard sync.Mutex

	app := &App{
		runtime:     rt,
		session:     session.New(),
		events:      make(chan tea.Msg, 16),
		streamGuard: &guard,
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	app.Run(ctx, cancel, "hello", nil)

	// isLocked probes guard without leaking a spurious lock acquisition:
	// TryLock succeeding would otherwise leave the test goroutine holding
	// the mutex, making the later "released" check hang forever.
	isLocked := func() bool {
		if guard.TryLock() {
			guard.Unlock()
			return false
		}
		return true
	}

	require.Eventually(t, isLocked, time.Second, time.Millisecond, "streamGuard should be held while RunStream is active")

	close(release)

	require.Eventually(t, func() bool { return !isLocked() }, time.Second, time.Millisecond, "streamGuard should be released once RunStream ends")
}

// TestApp_AcquireStreamGuard_NoopWhenUnset verifies that a bare App with no
// WithStreamGuard option (the common case: no attached SessionManager) never
// blocks on a nil lock.
func TestApp_AcquireStreamGuard_NoopWhenUnset(t *testing.T) {
	t.Parallel()

	app := &App{}
	release := app.acquireStreamGuard()
	require.NotPanics(t, release)
}

func TestApp_Retry_SuppressesReEmittedUserMessage(t *testing.T) {
	t.Parallel()

	events := make(chan tea.Msg, 16)
	app := &App{
		runtime: &retryMockRuntime{},
		session: session.New(),
		events:  events,
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	app.Retry(ctx, cancel)

	var userMessages []string
	var sawStreamStarted, sawStreamStopped bool
	deadline := time.After(2 * time.Second)
	for !sawStreamStopped {
		select {
		case ev := <-events:
			switch e := ev.(type) {
			case *runtime.UserMessageEvent:
				userMessages = append(userMessages, e.Message)
			case *runtime.StreamStartedEvent:
				sawStreamStarted = true
			case *runtime.StreamStoppedEvent:
				sawStreamStopped = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for StreamStopped")
		}
	}

	assert.True(t, sawStreamStarted, "StreamStarted should be forwarded")
	// The pre-StreamStarted re-emission is dropped; the post-StreamStarted
	// (steered) user message is kept.
	assert.Equal(t, []string{"steered"}, userMessages,
		"only the post-StreamStarted user message should be forwarded")
}

// backgroundEventMockRuntime captures the handler App.Start registers via
// OnBackgroundEvent so tests can emit background events through it.
type backgroundEventMockRuntime struct {
	mockRuntime

	handler func(runtime.Event)
}

func (m *backgroundEventMockRuntime) OnBackgroundEvent(handler func(runtime.Event)) {
	m.handler = handler
}

// TestApp_Start_ForwardsBackgroundEvents verifies Start wires the runtime's
// out-of-band background-event hook into the app's event stream, so token
// usage from background agent tasks reaches the TUI subscribers.
func TestApp_Start_ForwardsBackgroundEvents(t *testing.T) {
	t.Parallel()

	rt := &backgroundEventMockRuntime{}
	events := make(chan tea.Msg, 16)
	app := &App{
		runtime: rt,
		session: session.New(),
		events:  events,
	}

	app.Start(t.Context())
	require.NotNil(t, rt.handler, "Start must register the background-event handler")

	usage := runtime.NewTokenUsageEvent("bg-session", "worker", &runtime.Usage{
		ContextLength: 150,
		ContextLimit:  1000,
	})
	rt.handler(usage)

	select {
	case msg := <-events:
		assert.Equal(t, usage, msg, "the background event must reach the app's event stream unchanged")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the forwarded background event")
	}
}

// elicitationRequestMockRuntime captures the handler App.Start registers via
// OnElicitationRequest and records every ResumeElicitation call, so tests can
// drive the sink and assert on what App forwards back to the runtime.
type elicitationRequestMockRuntime struct {
	mockRuntime

	handler func(runtime.Event)

	mu             sync.Mutex
	resumedIDs     []string
	resumedActions []tools.ElicitationAction
}

// firstOrEmpty returns the first element of ids, or "" when empty. Mirrors
// runtime.firstElicitationID for tests that record a variadic call's ID.
func firstOrEmpty(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func (m *elicitationRequestMockRuntime) OnElicitationRequest(handler func(runtime.Event)) {
	m.handler = handler
}

func (m *elicitationRequestMockRuntime) ResumeElicitation(_ context.Context, action tools.ElicitationAction, _ map[string]any, elicitationID ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resumedIDs = append(m.resumedIDs, firstOrEmpty(elicitationID))
	m.resumedActions = append(m.resumedActions, action)
	return nil
}

// TestApp_Start_ForwardsElicitationRequests verifies Start wires the
// runtime's OnElicitationRequest sink into the app's event stream, so
// background-job elicitations (which have no live channel of their own
// reaching the TUI) are surfaced (#3584).
func TestApp_Start_ForwardsElicitationRequests(t *testing.T) {
	t.Parallel()

	rt := &elicitationRequestMockRuntime{}
	events := make(chan tea.Msg, 16)
	app := &App{
		runtime: rt,
		session: session.New(),
		events:  events,
	}

	app.Start(t.Context())
	require.NotNil(t, rt.handler, "Start must register the OnElicitationRequest handler")

	ev := runtime.ElicitationRequest("need input", "form", nil, "", "eid-1", "", "sess-1", nil, "worker")
	rt.handler(ev)

	select {
	case msg := <-events:
		assert.Equal(t, ev, msg, "the elicitation request must reach the app's event stream unchanged")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the forwarded elicitation request")
	}
}

// TestApp_SendEvent_DeliversElicitationWithoutDedupe pins the #3584 fix that
// removed the App-side ElicitationID dedupe: the runtime's
// OnElicitationRequest sink is now the single, exactly-once delivery point
// (elicitationHandler calls it directly, synchronously, and unconditionally,
// and runCollecting no longer re-forwards a bridge-observed copy), so
// sendEvent must forward every event unconditionally — including two
// distinct events that happen to share an ElicitationID, which a stateful
// dedupe would have incorrectly collapsed.
func TestApp_SendEvent_DeliversElicitationWithoutDedupe(t *testing.T) {
	t.Parallel()

	events := make(chan tea.Msg, 16)
	app := &App{events: events}
	ctx := t.Context()

	ev := runtime.ElicitationRequest("need input", "form", nil, "", "eid-dup", "", "sess-1", nil, "worker")
	app.sendEvent(ctx, ev)
	require.Len(t, events, 1, "the sink's single delivery must reach the app's event stream")
	<-events

	// A second event that happens to carry the same ElicitationID (e.g. a
	// canceled request's ID reused later) must still go through: nothing in
	// the App layer keys off ElicitationID any more.
	app.sendEvent(ctx, ev)
	require.Len(t, events, 1, "sendEvent must not drop a delivery based on ElicitationID")
}

// TestApp_ResumeElicitation_ForwardsID verifies ResumeElicitation passes the
// elicitation ID through to the runtime unchanged. There is no dedupe state
// to clear any more (#3584): the runtime's per-request waiter registry is
// the sole source of truth for whether an ID is still answerable.
func TestApp_ResumeElicitation_ForwardsID(t *testing.T) {
	t.Parallel()

	rt := &elicitationRequestMockRuntime{}
	app := &App{runtime: rt, events: make(chan tea.Msg, 16)}

	require.NoError(t, app.ResumeElicitation(t.Context(), tools.ElicitationActionAccept, nil, "eid-clear"))

	require.Len(t, rt.resumedIDs, 1)
	assert.Equal(t, "eid-clear", rt.resumedIDs[0])
	assert.Equal(t, tools.ElicitationActionAccept, rt.resumedActions[0])
}

// stubSnapshotController is a tiny SnapshotController used by the app
// tests to drive /undo without spinning up a real shadow-git
// repository. enabled gates SnapshotsEnabled(), and the (files, ok,
// err) tuple is returned verbatim from UndoLast / Reset so each test
// can assert the result-shaping logic in [snapshotResult].
type stubSnapshotController struct {
	enabled bool
	files   int
	ok      bool
	err     error
}

func (s *stubSnapshotController) Enabled() bool { return s.enabled }
func (s *stubSnapshotController) UndoLast(context.Context, string, string) (int, bool, error) {
	return s.files, s.ok, s.err
}

func (s *stubSnapshotController) List(string) []builtins.SnapshotInfo { return nil }
func (s *stubSnapshotController) Reset(context.Context, string, string, int) (int, bool, error) {
	return s.files, s.ok, s.err
}
func (s *stubSnapshotController) AutoInject(*hooks.Config) {}

var _ builtins.SnapshotController = (*stubSnapshotController)(nil)

func TestApp_NewSession_PreservesToolsApproved(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{}

	// Create initial session with tools approved
	initialSess := session.New(session.WithToolsApproved(true))
	require.True(t, initialSess.ToolsApproved, "Initial session should have tools approved")

	app := New(t.Context(), rt, initialSess)

	// Call NewSession - should preserve ToolsApproved
	app.NewSession()

	assert.True(t, app.Session().ToolsApproved, "NewSession should preserve ToolsApproved")
}

// Explicit strict/balanced modes leave ToolsApproved=false, so preserving
// only that legacy flag silently dropped the mode on /new.
func TestApp_NewSession_PreservesSafetyPolicy(t *testing.T) {
	t.Parallel()

	for _, policy := range []session.SafetyPolicy{session.SafetyPolicyStrict, session.SafetyPolicyBalanced} {
		t.Run(string(policy), func(t *testing.T) {
			t.Parallel()

			rt := &mockRuntime{}

			initialSess := session.New(session.WithSafetyPolicy(policy))
			require.Equal(t, policy, initialSess.GetSafetyPolicy())

			app := New(t.Context(), rt, initialSess)

			app.NewSession()

			assert.Equal(t, policy, app.Session().GetSafetyPolicy(), "NewSession should preserve the safety policy")
			assert.False(t, app.Session().ToolsApproved, "non-autonomous modes must not grant blanket approval")
		})
	}
}

// An autonomous escalation must keep its toggle-back destination across
// /new, or toggling yolo off afterwards drops to the legacy default
// instead of the mode the user escalated from.
func TestApp_NewSession_PreservesPriorSafetyPolicy(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{}

	initialSess := session.New(session.WithSafetyPolicy(session.SafetyPolicyBalanced))
	initialSess.ToggleYolo() // escalate: autonomous with prior=balanced
	require.Equal(t, session.SafetyPolicyAutonomous, initialSess.GetSafetyPolicy())
	require.Equal(t, session.SafetyPolicyBalanced, initialSess.GetPriorSafetyPolicy())

	app := New(t.Context(), rt, initialSess)

	app.NewSession()

	sess := app.Session()
	assert.Equal(t, session.SafetyPolicyAutonomous, sess.GetSafetyPolicy(), "NewSession should preserve the escalated mode")
	assert.Equal(t, session.SafetyPolicyBalanced, sess.GetPriorSafetyPolicy(), "NewSession should preserve the toggle-back memory")

	// The preserved memory must actually work: toggling off restores balanced.
	sess.ToggleYolo()
	assert.Equal(t, session.SafetyPolicyBalanced, sess.GetSafetyPolicy())
}

func TestApp_NewSession_PreservesHideToolResults(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{}

	// Create initial session with hide tool results
	initialSess := session.New(session.WithHideToolResults(true))
	require.True(t, initialSess.HideToolResults, "Initial session should have HideToolResults")

	app := New(t.Context(), rt, initialSess)

	// Call NewSession - should preserve HideToolResults
	app.NewSession()

	assert.True(t, app.Session().HideToolResults, "NewSession should preserve HideToolResults")
}

func TestApp_NewSession_WithNilSession(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{}

	// Create app with nil session (edge case)
	app := &App{
		ctx:     t.Context,
		runtime: rt,
		session: nil,
	}

	// Call NewSession - should not panic and create a new session with defaults
	app.NewSession()

	require.NotNil(t, app.Session(), "NewSession should create a new session")
	assert.False(t, app.Session().ToolsApproved, "NewSession with nil should use default ToolsApproved=false")
}

func TestApp_UpdateSessionTitle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	t.Run("updates title in session", func(t *testing.T) {
		t.Parallel()

		rt := &mockRuntime{}
		sess := session.New()
		events := make(chan tea.Msg, 16)
		app := &App{
			runtime: rt,
			session: sess,
			events:  events,
		}

		err := app.UpdateSessionTitle(ctx, "New Title")
		require.NoError(t, err)

		assert.Equal(t, "New Title", sess.Title)

		// Check that an event was emitted
		select {
		case event := <-events:
			titleEvent, ok := event.(*runtime.SessionTitleEvent)
			require.True(t, ok, "should emit SessionTitleEvent")
			assert.Equal(t, "New Title", titleEvent.Title)
		default:
			t.Fatal("expected SessionTitleEvent to be emitted")
		}
	})

	t.Run("returns error when no session", func(t *testing.T) {
		t.Parallel()

		rt := &mockRuntime{}
		app := &App{
			runtime: rt,
			session: nil,
		}

		err := app.UpdateSessionTitle(ctx, "New Title")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no active session")
	})

	t.Run("returns ErrTitleGenerating when generation in progress", func(t *testing.T) {
		t.Parallel()

		rt := &mockRuntime{}
		sess := session.New()
		events := make(chan tea.Msg, 16)
		app := &App{
			runtime: rt,
			session: sess,
			events:  events,
		}

		// Simulate title generation in progress
		app.titleGenerating.Store(true)

		err := app.UpdateSessionTitle(ctx, "New Title")
		require.ErrorIs(t, err, ErrTitleGenerating)

		// Title should not be updated
		assert.Empty(t, sess.Title)
	})
}

func TestApp_ResolveSkillCommand_NoLocalRuntime(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rt := &mockRuntime{}
	sess := session.New()
	app := New(t.Context(), rt, sess)

	// mockRuntime is not a LocalRuntime, so no skills should be returned
	resolved, err := app.ResolveSkillCommand(ctx, "/some-skill")
	require.NoError(t, err)
	assert.Empty(t, resolved)
}

func TestApp_ResolveSkillCommand_NotSlashCommand(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rt := &mockRuntime{}
	sess := session.New()
	app := New(t.Context(), rt, sess)

	resolved, err := app.ResolveSkillCommand(ctx, "not a slash command")
	require.NoError(t, err)
	assert.Empty(t, resolved)
}

func TestApp_UndoLastSnapshot(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	app := New(t.Context(), &mockRuntime{}, session.New(),
		WithSnapshotController(&stubSnapshotController{enabled: true, files: 2, ok: true}),
	)
	result, err := app.UndoLastSnapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, result.RestoredFiles)
}

func TestApp_UndoLastSnapshot_NoSnapshot(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	app := New(t.Context(), &mockRuntime{}, session.New(),
		WithSnapshotController(&stubSnapshotController{enabled: true}),
	)
	_, err := app.UndoLastSnapshot(ctx)
	assert.ErrorIs(t, err, ErrNothingToUndo)
}

func TestApp_UndoLastSnapshot_NoController(t *testing.T) {
	t.Parallel()

	// Without a SnapshotController the App reports nothing to undo,
	// so the same UI affordance can light up regardless of which
	// runtime the embedder paired the App with.
	ctx := t.Context()
	app := New(t.Context(), &mockRuntime{}, session.New())
	_, err := app.UndoLastSnapshot(ctx)
	require.ErrorIs(t, err, ErrNothingToUndo)
	assert.False(t, app.SnapshotsEnabled())
}

func TestApp_SnapshotsEnabled_DoesNotRequireSession(t *testing.T) {
	t.Parallel()

	// SnapshotsEnabled answers a controller-capability question; it
	// must not silently return false just because no session is attached.
	app := &App{
		ctx:                t.Context,
		runtime:            &mockRuntime{},
		session:            nil,
		snapshotController: &stubSnapshotController{enabled: true},
	}
	assert.True(t, app.SnapshotsEnabled())
}

func TestApp_SubscribeWith_FanOutToMultipleSubscribers(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	rt := &mockRuntime{}
	app := New(t.Context(), rt, session.New())

	recv := func() (chan tea.Msg, context.CancelFunc) {
		subCtx, subCancel := context.WithCancel(ctx)
		ch := make(chan tea.Msg, 16)
		go app.SubscribeWith(subCtx, func(m tea.Msg) { ch <- m })
		return ch, subCancel
	}

	a, cancelA := recv()
	b, cancelB := recv()
	defer cancelA()
	defer cancelB()

	// Wait until both subscribers are registered before publishing.
	require.Eventually(t, func() bool {
		app.subsMu.Lock()
		defer app.subsMu.Unlock()
		return len(app.subs) == 2
	}, time.Second, 5*time.Millisecond)

	app.events <- runtime.SessionTitle("sess", "hello")

	for _, ch := range []chan tea.Msg{a, b} {
		select {
		case msg := <-ch:
			ev, ok := msg.(*runtime.SessionTitleEvent)
			require.True(t, ok)
			assert.Equal(t, "hello", ev.Title)
		case <-time.After(time.Second):
			t.Fatal("subscriber did not receive event")
		}
	}
}

func TestApp_RegenerateSessionTitle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	t.Run("returns error when no session", func(t *testing.T) {
		t.Parallel()

		rt := &mockRuntime{}
		app := &App{
			runtime: rt,
			session: nil,
		}

		err := app.RegenerateSessionTitle(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no active session")
	})

	t.Run("returns error when no title generator is available", func(t *testing.T) {
		t.Parallel()

		rt := &mockRuntime{}
		sess := session.New()
		events := make(chan tea.Msg, 16)
		app := &App{
			runtime: rt,
			session: sess,
			events:  events,
			// titleGen is nil - no title generator available
		}

		err := app.RegenerateSessionTitle(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "title regeneration not available")
	})

	t.Run("returns ErrTitleGenerating when already generating", func(t *testing.T) {
		t.Parallel()

		rt := &mockRuntime{}
		sess := session.New()
		events := make(chan tea.Msg, 16)
		app := &App{
			runtime: rt,
			session: sess,
			events:  events,
		}

		// Simulate title generation already in progress
		app.titleGenerating.Store(true)

		err := app.RegenerateSessionTitle(ctx)
		require.ErrorIs(t, err, ErrTitleGenerating)
	})
}

// TestApp_InjectUserMessage verifies a follow-up injected by an external
// driver is published on the event bus as a SendMsg — the same message the
// TUI produces when the user submits input — so it flows through the normal
// run path (queueing, title generation, event streaming).
func TestApp_InjectUserMessage(t *testing.T) {
	t.Parallel()

	events := make(chan tea.Msg, 4)
	app := &App{
		ctx:     t.Context,
		runtime: &mockRuntime{},
		session: session.New(),
		events:  events,
	}

	app.InjectUserMessage(t.Context(), "do the thing")

	select {
	case msg := <-events:
		sendMsg, ok := msg.(messages.SendMsg)
		require.True(t, ok, "should emit a SendMsg, got %T", msg)
		assert.Equal(t, "do the thing", sendMsg.Content)
	default:
		t.Fatal("expected a SendMsg to be emitted")
	}
}

func TestApp_DropAttachedFile(t *testing.T) {
	t.Parallel()
	abs := t.TempDir()
	foo := filepath.Join(abs, "foo.go")
	bar := filepath.Join(abs, "bar.go")

	newAppWithAttachments := func(store session.Store, paths ...string) (*App, *session.Session) {
		sess := session.New(session.WithAttachedFiles(paths))
		return New(t.Context(), &mockRuntime{store: store}, sess), sess
	}

	t.Run("drops by exact path and syncs the store", func(t *testing.T) {
		t.Parallel()
		store := session.NewInMemorySessionStore()
		app, sess := newAppWithAttachments(store, foo, bar)

		dropped, err := app.DropAttachedFile(t.Context(), foo)
		require.NoError(t, err)
		assert.Equal(t, foo, dropped)
		assert.Equal(t, []string{bar}, sess.AttachedFilesSnapshot())

		stored, err := store.GetSession(t.Context(), sess.ID)
		require.NoError(t, err)
		assert.Equal(t, []string{bar}, stored.AttachedFilesSnapshot())
	})

	t.Run("drops by unique base name", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(abs, "dir")
		foo := filepath.Join(dir, "foo.go")
		bar := filepath.Join(dir, "bar.go")
		app, sess := newAppWithAttachments(nil, foo, bar)

		dropped, err := app.DropAttachedFile(t.Context(), "foo.go")
		require.NoError(t, err)
		assert.Equal(t, foo, dropped)
		assert.Equal(t, []string{bar}, sess.AttachedFilesSnapshot())
	})

	t.Run("rejects ambiguous base names", func(t *testing.T) {
		t.Parallel()
		app, sess := newAppWithAttachments(nil, filepath.Join(abs, "a", "foo.go"), filepath.Join(abs, "b", "foo.go"))

		_, err := app.DropAttachedFile(t.Context(), "foo.go")
		require.ErrorContains(t, err, "matches 2 attached files")
		assert.Len(t, sess.AttachedFilesSnapshot(), 2)
	})

	t.Run("rejects unknown files and blank input", func(t *testing.T) {
		t.Parallel()
		app, _ := newAppWithAttachments(nil, foo)

		_, err := app.DropAttachedFile(t.Context(), filepath.Join(abs, "other.go"))
		require.ErrorContains(t, err, "not attached")

		_, err = app.DropAttachedFile(t.Context(), "   ")
		require.ErrorContains(t, err, "no file specified")
	})

	t.Run("reports when nothing is attached", func(t *testing.T) {
		t.Parallel()
		app, _ := newAppWithAttachments(nil)

		_, err := app.DropAttachedFile(t.Context(), "foo.go")
		require.ErrorContains(t, err, "no files are attached")
	})

	t.Run("returns error when no session", func(t *testing.T) {
		t.Parallel()
		app := &App{runtime: &mockRuntime{}}

		_, err := app.DropAttachedFile(t.Context(), "foo.go")
		require.ErrorContains(t, err, "no active session")
	})
}

func TestResolveAttachedFile_RelativePath(t *testing.T) {
	t.Parallel()

	abs, err := filepath.Abs("notes.md")
	require.NoError(t, err)

	resolved, err := resolveAttachedFile([]string{abs}, "notes.md")
	require.NoError(t, err)
	assert.Equal(t, abs, resolved)
}

// liveSessionsMockRuntime layers the optional live-session capabilities
// (LiveSessions, CompactLiveSession) over the base mock runtime.
type liveSessionsMockRuntime struct {
	mockRuntime

	rows        []runtime.LiveSession
	lastCurrent *session.Session
	compactedID string
	compactErr  error
}

func (m *liveSessionsMockRuntime) LiveSessions(_ context.Context, current *session.Session) []runtime.LiveSession {
	m.lastCurrent = current
	return m.rows
}

func (m *liveSessionsMockRuntime) CompactLiveSession(_ context.Context, sessionID, _ string, events runtime.EventSink) error {
	if m.compactErr != nil {
		return m.compactErr
	}
	m.compactedID = sessionID
	events.Emit(runtime.SessionCompactionCompleted(sessionID, runtime.CompactionOutcomeApplied, "worker"))
	return nil
}

// thinkingLevelsMockRuntime is a minimal mockRuntime extension implementing
// agentThinkingLevelsProvider, exercising the pass-through half of
// App.CurrentAgentThinkingLevels (the type-assertion-miss half is covered by
// plain *mockRuntime, which does not implement the interface).
type thinkingLevelsMockRuntime struct {
	mockRuntime

	levels []effort.Level
}

func (m *thinkingLevelsMockRuntime) CurrentAgentThinkingLevels(context.Context) []effort.Level {
	return m.levels
}

func TestApp_CurrentAgentThinkingLevels_UnsupportedRuntime(t *testing.T) {
	t.Parallel()

	app := New(t.Context(), &mockRuntime{}, session.New())
	assert.Nil(t, app.CurrentAgentThinkingLevels(t.Context()),
		"runtimes without a thinking-levels resolution degrade to no /effort candidates")
}

func TestApp_CurrentAgentThinkingLevels_PassesThroughRuntimeLevels(t *testing.T) {
	t.Parallel()

	rt := &thinkingLevelsMockRuntime{levels: []effort.Level{effort.Low, effort.Medium, effort.High}}
	app := New(t.Context(), rt, session.New())

	assert.Equal(t, []effort.Level{effort.Low, effort.Medium, effort.High}, app.CurrentAgentThinkingLevels(t.Context()))
}

func TestApp_LiveSessions_UnsupportedRuntime(t *testing.T) {
	t.Parallel()

	app := New(t.Context(), &mockRuntime{}, session.New())
	assert.Nil(t, app.LiveSessions(t.Context()),
		"runtimes without live-session tracking degrade to an empty team view")
}

func TestApp_CompactLiveSession_UnsupportedRuntime(t *testing.T) {
	t.Parallel()

	app := New(t.Context(), &mockRuntime{}, session.New())
	err := app.CompactLiveSession(t.Context(), "some-session", "")
	require.ErrorIs(t, err, runtime.ErrUnsupported)
}

func TestApp_LiveSessions_PassesCurrentSession(t *testing.T) {
	t.Parallel()

	sess := session.New()
	rt := &liveSessionsMockRuntime{rows: []runtime.LiveSession{
		{SessionID: sess.ID, AgentName: "root", Current: true},
		{SessionID: "child-1", AgentName: "worker"},
	}}
	app := New(t.Context(), rt, sess)

	rows := app.LiveSessions(t.Context())
	require.Len(t, rows, 2)
	assert.Same(t, sess, rt.lastCurrent, "the app's current session drives the root row")
}

func TestApp_CompactLiveSession_BridgesEventsIntoStream(t *testing.T) {
	t.Parallel()

	rt := &liveSessionsMockRuntime{}
	app := New(t.Context(), rt, session.New())

	require.NoError(t, app.CompactLiveSession(t.Context(), "child-1", ""))
	assert.Equal(t, "child-1", rt.compactedID)

	select {
	case msg := <-app.events:
		evt, ok := msg.(*runtime.SessionCompactionEvent)
		require.True(t, ok, "expected SessionCompactionEvent, got %T", msg)
		assert.Equal(t, "child-1", evt.SessionID)
		assert.Equal(t, "completed", evt.Status)
	case <-time.After(time.Second):
		t.Fatal("compaction event was not bridged into the app event stream")
	}
}

func TestApp_CompactLiveSession_ForwardsRuntimeError(t *testing.T) {
	t.Parallel()

	rt := &liveSessionsMockRuntime{compactErr: errors.New("session x is not live")}
	app := New(t.Context(), rt, session.New())

	err := app.CompactLiveSession(t.Context(), "x", "")
	require.ErrorContains(t, err, "not live")
}

// composedElicitationMockRuntime reproduces the production wiring that
// exists once both the OnElicitationRequest sink (registered by App.Start)
// and a live RunStream (read directly by App.Run/Retry/RunWithMessage) are
// present for the same foreground request. LocalRuntime.elicitationHandler
// delivers a request to the sink synchronously and exactly once, then
// separately best-effort-sends the same event on the elicitation bridge —
// which, for a foreground stream, is the very channel RunStream returns.
// This mock drives both paths the same way so tests can assert on the
// combined result App actually observes (#3584).
type composedElicitationMockRuntime struct {
	mockRuntime

	handler func(runtime.Event)
}

func (m *composedElicitationMockRuntime) OnElicitationRequest(handler func(runtime.Event)) {
	m.handler = handler
}

// MirrorsElicitationOnRunStream marks this mock as reproducing LocalRuntime's
// mirrored-delivery contract (runtime.LocalRuntime.MirrorsElicitationOnRunStream),
// so App's RunStream-forwarding loops must skip the duplicate copy this mock
// also pushes onto RunStream below.
func (m *composedElicitationMockRuntime) MirrorsElicitationOnRunStream() {}

func (m *composedElicitationMockRuntime) RunStream(_ context.Context, sess *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event, 8)
	go func() {
		defer close(ch)
		ch <- runtime.StreamStarted(sess.ID, "mock")

		ev := runtime.ElicitationRequest("need input", "form", nil, "", "eid-1", "", sess.ID, nil, "mock")
		if m.handler != nil {
			m.handler(ev) // reliable sink delivery (elicitationHandler, #3584)
		}
		ch <- ev // best-effort bridge delivery on the same RunStream channel

		ch <- runtime.StreamStopped(sess.ID, "mock", "normal")
	}()
	return ch
}

// remoteLikeMockRuntime mirrors RemoteRuntime's elicitation contract: its
// OnElicitationRequest sink is a no-op (mockRuntime's default; see
// pkg/runtime/remote_runtime.go's OnElicitationRequest) and every
// elicitation request arrives ONLY as a *runtime.ElicitationRequestEvent on
// the RunStream channel, exactly like remote_runtime.go's RunStream
// forwarding loop. It deliberately does NOT implement
// elicitationSinkMirror, so App must not skip this event for it.
type remoteLikeMockRuntime struct {
	mockRuntime
}

func (m *remoteLikeMockRuntime) RunStream(_ context.Context, sess *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event, 8)
	go func() {
		defer close(ch)
		ch <- runtime.StreamStarted(sess.ID, "mock")
		ch <- runtime.ElicitationRequest("need input", "form", nil, "", "eid-1", "", sess.ID, nil, "mock")
		ch <- runtime.StreamStopped(sess.ID, "mock", "normal")
	}()
	return ch
}

// countElicitationDeliveries drains events until a *runtime.StreamStoppedEvent
// (or a 2s timeout) and returns how many *runtime.ElicitationRequestEvent
// values were observed along the way. Shared by every regression test below
// so the drain/timeout logic isn't re-authored (and potentially
// mis-authored) per entry point.
func countElicitationDeliveries(t *testing.T, events <-chan tea.Msg) int {
	t.Helper()

	n := 0
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-events:
			if _, ok := msg.(*runtime.ElicitationRequestEvent); ok {
				n++
			}
			if _, ok := msg.(*runtime.StreamStoppedEvent); ok {
				return n
			}
		case <-deadline:
			t.Fatal("timed out waiting for the stream to stop")
			return n
		}
	}
}

// TestReview_RemoteRuntimeElicitationIsStillDelivered is the regression probe
// for the over-broad fix to the #3584 double-delivery bug: commit 2ad095ae0
// made App.Run/Retry/RunWithMessage skip every
// *runtime.ElicitationRequestEvent read off RunStream UNCONDITIONALLY, which
// also silently dropped elicitations for runtimes whose OnElicitationRequest
// sink is a no-op and that deliver ONLY via RunStream (RemoteRuntime) — a
// remote-backed session showed zero dialogs instead of one. A runtime that
// does not mirror sink deliveries onto RunStream must have that event pass
// through to a.events untouched.
func TestReview_RemoteRuntimeElicitationIsStillDelivered(t *testing.T) {
	t.Parallel()

	rt := &remoteLikeMockRuntime{}
	events := make(chan tea.Msg, 16)
	app := &App{runtime: rt, session: session.New(), events: events}

	app.Start(t.Context())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	app.Run(ctx, cancel, "hello", nil)

	assert.Equal(t, 1, countElicitationDeliveries(t, events), "a remote/no-sink runtime's elicitation must still reach the app via RunStream")
}

// TestReview_ForegroundElicitationIsNotDeliveredTwice is the reviewer's
// regression probe for the #3584 double-delivery bug: a foreground
// LocalRuntime elicitation request reached a.events via BOTH the
// OnElicitationRequest sink (Start) and the RunStream forwarding loop
// (Run), producing two dialogs for one request. Exactly one
// ElicitationRequestEvent must reach the app's event stream per request.
func TestReview_ForegroundElicitationIsNotDeliveredTwice(t *testing.T) {
	t.Parallel()

	rt := &composedElicitationMockRuntime{}
	events := make(chan tea.Msg, 16)
	app := &App{runtime: rt, session: session.New(), events: events}

	app.Start(t.Context())
	require.NotNil(t, rt.handler, "Start must register the OnElicitationRequest handler")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	app.Run(ctx, cancel, "hello", nil)

	assert.Equal(t, 1, countElicitationDeliveries(t, events), "a single foreground elicitation must open exactly one dialog")
}

// TestMustSkipMirroredElicitation_ConcreteRuntimeClassification pins
// mustSkipMirroredElicitation's classification of the two CONCRETE
// production runtimes, not just marker-shaped mocks. The composed/remote
// regression tests above exercise independent mock types that each
// hand-declare whether they implement elicitationSinkMirror; they never
// touch runtime.LocalRuntime or runtime.RemoteRuntime, so an inverted
// production mapping (marker removed from LocalRuntime, or added to
// RemoteRuntime) would leave every other test in this file green (#3584
// review — mutation testing proof). *runtime.LocalRuntime and
// *runtime.RemoteRuntime are asserted directly here; a nil pointer of each
// concrete type is enough since mustSkipMirroredElicitation only performs a
// type assertion and never calls a method on rt.
func TestMustSkipMirroredElicitation_ConcreteRuntimeClassification(t *testing.T) {
	t.Parallel()

	assert.True(t, mustSkipMirroredElicitation((*runtime.LocalRuntime)(nil)),
		"LocalRuntime mirrors OnElicitationRequest sink deliveries onto RunStream; App must skip the duplicate RunStream copy")
	assert.False(t, mustSkipMirroredElicitation((*runtime.RemoteRuntime)(nil)),
		"RemoteRuntime delivers elicitations ONLY via RunStream (its OnElicitationRequest sink is a no-op); App must not skip that copy")
}

// elicitationEntryPoint names one of App's three RunStream-forwarding entry
// points and how to invoke it, so the delivery coverage below can drive all
// three through one table instead of duplicating the drive/drain logic.
type elicitationEntryPoint struct {
	name   string
	invoke func(app *App, ctx context.Context, cancel context.CancelFunc)
}

var elicitationEntryPoints = []elicitationEntryPoint{
	{"Run", func(app *App, ctx context.Context, cancel context.CancelFunc) {
		app.Run(ctx, cancel, "hello", nil)
	}},
	{"Retry", func(app *App, ctx context.Context, cancel context.CancelFunc) {
		app.Retry(ctx, cancel)
	}},
	{"RunWithMessage", func(app *App, ctx context.Context, cancel context.CancelFunc) {
		app.RunWithMessage(ctx, cancel, session.UserMessage("hello"))
	}},
}

// TestElicitationDeliveryAcrossEntryPoints is the mutation-hardened
// regression coverage for the #3584 double-delivery fix across ALL THREE
// RunStream-forwarding entry points. Before forwardRunStreamEvents
// centralized the gating logic, Run, Retry, and RunWithMessage each carried
// their own independent copy of it — only Run was ever driven through a
// mirrored and an unmirrored runtime, so breaking delivery in Retry (e.g.
// dropping the remote/unmirrored copy) or RunWithMessage (e.g. failing to
// dedupe the mirrored copy) left `go test ./pkg/app` green (#3584 review,
// mutation-testing proof). Every entry point must deliver exactly one
// dialog for both a mirrored/local-shaped runtime (sink AND RunStream both
// fire; the RunStream copy must be skipped) and an unmirrored/remote-shaped
// runtime (RunStream is the only delivery; it must pass through).
func TestElicitationDeliveryAcrossEntryPoints(t *testing.T) {
	t.Parallel()

	for _, ep := range elicitationEntryPoints {
		t.Run(ep.name, func(t *testing.T) {
			t.Parallel()

			t.Run("mirrored runtime delivers exactly once", func(t *testing.T) {
				t.Parallel()

				rt := &composedElicitationMockRuntime{}
				events := make(chan tea.Msg, 16)
				app := &App{runtime: rt, session: session.New(), events: events}
				app.Start(t.Context())
				require.NotNil(t, rt.handler, "Start must register the OnElicitationRequest handler")

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				ep.invoke(app, ctx, cancel)

				assert.Equal(t, 1, countElicitationDeliveries(t, events),
					"a mirrored/local-shaped runtime's foreground elicitation must open exactly one dialog")
			})

			t.Run("unmirrored runtime still delivers once", func(t *testing.T) {
				t.Parallel()

				rt := &remoteLikeMockRuntime{}
				events := make(chan tea.Msg, 16)
				app := &App{runtime: rt, session: session.New(), events: events}
				app.Start(t.Context())

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				ep.invoke(app, ctx, cancel)

				assert.Equal(t, 1, countElicitationDeliveries(t, events),
					"an unmirrored/remote-shaped runtime's elicitation must still reach the app via RunStream")
			})
		})
	}
}
