package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/tuitest"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// storeRuntime is stubRuntime plus a session store, so the /sessions flow
// (LoadSessionMsg) can resolve past sessions.
type storeRuntime struct {
	stubRuntime

	store session.Store
}

func (r storeRuntime) SessionStore() session.Store { return r.store }

func TestLoadSessionThenClickEditLabel(t *testing.T) {
	t.Run("in-place", func(t *testing.T) { testLoadSessionThenClickEditLabel(t, false, 120) })
	t.Run("new tab", func(t *testing.T) { testLoadSessionThenClickEditLabel(t, true, 120) })
	// Narrow terminal: the sidebar renders as a horizontal band above the
	// chat. Startup info arriving after the load grows the band, shifting the
	// messages down; hit-testing must follow.
	t.Run("narrow band layout", func(t *testing.T) { testLoadSessionThenClickEditLabel(t, false, 100) })
}

func testLoadSessionThenClickEditLabel(t *testing.T, newTab bool, width int) {
	t.Helper()
	dir := t.TempDir()
	paths.SetDataDir(filepath.Join(dir, "data"))
	paths.SetConfigDir(filepath.Join(dir, "config"))
	t.Cleanup(func() {
		paths.SetDataDir("")
		paths.SetConfigDir("")
	})

	ctx := t.Context()
	store := session.NewInMemorySessionStore()

	// A past session with a user and an assistant message.
	past := session.New()
	past.AddMessage(session.UserMessage("hello there"))
	past.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "general kenobi",
	}))
	require.NoError(t, store.AddSession(ctx, past))

	rt := storeRuntime{store: store}
	initialSess := session.New()
	if newTab {
		// A non-empty current session forces handleLoadSession to open the
		// past session in a new tab instead of replacing in-place.
		initialSess.AddMessage(session.UserMessage("existing chat"))
	}
	application := app.New(ctx, rt, initialSess)

	spawner := func(ctx context.Context, workingDir string) (*app.App, *session.Session, func(), error) {
		sess := session.New()
		return app.New(ctx, rt, sess), sess, func() {}, nil
	}

	model := New(ctx, spawner, application, dir, func() {})
	m := model.(*appModel)
	// New opened the tui_state.db SQLite store under dir/data; release it
	// through the model's shutdown path so t.TempDir cleanup can delete the
	// file on Windows.
	t.Cleanup(m.cleanupManagedResources)

	driver := tuitest.New(t, model, width, 40)

	// Load the past session (what the /sessions browser emits).
	driver.Send(messages.LoadSessionMsg{SessionID: past.ID})
	driver.WaitFor(tuitest.Contains("hello there"))

	// Simulate the async startup info that App.ReplaceSession re-emits after
	// the load completed: it changes the collapsed sidebar band's height.
	driver.Send(&runtime.TeamInfoEvent{
		AvailableAgents: []runtime.AgentDetails{
			{Name: "root", Model: "gpt-4", Description: "root agent"},
			{Name: "helper", Model: "gpt-4", Description: "helper agent"},
		},
		CurrentAgent: "root",
	})

	frame := driver.Frame()
	t.Logf("frame after load:\n%s", ansi.Strip(frame))

	// Find the user message on screen.
	lines := strings.Split(ansi.Strip(frame), "\n")
	userLine := -1
	for i, l := range lines {
		if strings.Contains(l, "hello there") {
			userLine = i
			break
		}
	}
	require.GreaterOrEqual(t, userLine, 0, "user message must be visible")

	// Hover over the user message to reveal the action labels.
	driver.Send(tea.MouseMotionMsg{X: 10, Y: userLine})
	frame = driver.Frame()
	lines = strings.Split(ansi.Strip(frame), "\n")

	editLine, editCol := -1, -1
	for i, l := range lines {
		if idx := strings.Index(l, types.UserMessageEditLabel); idx >= 0 {
			editLine, editCol = i, idx
			break
		}
	}
	require.GreaterOrEqual(t, editLine, 0, "edit label should be visible after hover:\n%s", ansi.Strip(frame))
	t.Logf("edit label at screen line=%d col=%d", editLine, editCol)

	// Click on the edit label and wait for the asynchronously delivered edit command.
	driver.Send(tea.MouseClickMsg{X: editCol + 1, Y: editLine, Button: tea.MouseLeft})
	driver.WaitFor(tuitest.Contains("[editing]"))
}
