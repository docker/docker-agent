package app

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

// mockRuntime is a minimal mock for testing App without a real runtime
type mockRuntime struct{}

func (m *mockRuntime) CurrentAgentInfo(ctx context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{}
}
func (m *mockRuntime) CurrentAgentName() string          { return "mock" }
func (m *mockRuntime) SetCurrentAgent(name string) error { return nil }
func (m *mockRuntime) CurrentAgentTools(ctx context.Context) ([]tools.Tool, error) {
	return nil, nil
}

func (m *mockRuntime) EmitStartupInfo(ctx context.Context, sess *session.Session, events chan runtime.Event) {
}
func (m *mockRuntime) ResetStartupInfo() {}
func (m *mockRuntime) RunStream(ctx context.Context, sess *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event)
	close(ch)
	return ch
}

func (m *mockRuntime) Run(ctx context.Context, sess *session.Session) ([]session.Message, error) {
	return nil, nil
}
func (m *mockRuntime) Resume(ctx context.Context, req runtime.ResumeRequest) {}
func (m *mockRuntime) ResumeElicitation(ctx context.Context, action tools.ElicitationAction, content map[string]any) error {
	return nil
}
func (m *mockRuntime) SessionStore() session.Store { return nil }
func (m *mockRuntime) Summarize(ctx context.Context, sess *session.Session, additionalPrompt string, events chan runtime.Event) {
}
func (m *mockRuntime) PermissionsInfo() *runtime.PermissionsInfo { return nil }
func (m *mockRuntime) CurrentAgentSkillsToolset() *builtin.SkillsToolset {
	return nil
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
func (m *mockRuntime) TitleGenerator() *sessiontitle.Generator { return nil }
func (m *mockRuntime) Close() error                            { return nil }
func (m *mockRuntime) Stop()                                   {}
func (m *mockRuntime) Steer(_ runtime.QueuedMessage) error     { return nil }
func (m *mockRuntime) ClearSteerQueue()                        {}
func (m *mockRuntime) FollowUp(_ runtime.QueuedMessage) error  { return nil }
func (m *mockRuntime) ClearFollowUpQueue()                     {}

// Verify mockRuntime implements runtime.Runtime
var _ runtime.Runtime = (*mockRuntime)(nil)

func TestApp_NewSession_PreservesToolsApproved(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rt := &mockRuntime{}

	// Create initial session with tools approved
	initialSess := session.New(session.WithToolsApproved(true))
	require.True(t, initialSess.ToolsApproved, "Initial session should have tools approved")

	app := New(ctx, rt, initialSess)

	// Call NewSession - should preserve ToolsApproved
	app.NewSession()

	assert.True(t, app.Session().ToolsApproved, "NewSession should preserve ToolsApproved")
}

func TestApp_NewSession_PreservesHideToolResults(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rt := &mockRuntime{}

	// Create initial session with hide tool results
	initialSess := session.New(session.WithHideToolResults(true))
	require.True(t, initialSess.HideToolResults, "Initial session should have HideToolResults")

	app := New(ctx, rt, initialSess)

	// Call NewSession - should preserve HideToolResults
	app.NewSession()

	assert.True(t, app.Session().HideToolResults, "NewSession should preserve HideToolResults")
}

func TestApp_NewSession_WithNilSession(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{}

	// Create app with nil session (edge case)
	app := &App{
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
	app := New(ctx, rt, sess)

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
	app := New(ctx, rt, sess)

	resolved, err := app.ResolveSkillCommand(ctx, "not a slash command")
	require.NoError(t, err)
	assert.Empty(t, resolved)
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
