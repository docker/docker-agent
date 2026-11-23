package leantui

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
)

func TestSessionTitleEventSetsWindowTitle(t *testing.T) {
	t.Parallel()

	m := bareModel(24)
	m.app = app.New(t.Context(), &cycleThinkingRuntime{}, session.New(session.WithID("root")))

	var titles []string
	m.setWindowTitle = func(title string) {
		titles = append(titles, title)
	}

	m.handleEvent(t.Context(), runtime.SessionTitle("root", "Fix auth bug"))
	assert.Equal(t, []string{"docker agent: Fix auth bug"}, titles)
	assert.Equal(t, "Fix auth bug", m.sessionState.SessionTitle())

	// Duplicate title does not emit another terminal sequence.
	m.handleEvent(t.Context(), runtime.SessionTitle("root", "  Fix auth bug  "))
	assert.Equal(t, []string{"docker agent: Fix auth bug"}, titles)

	// Sub-session titles are ignored.
	m.handleEvent(t.Context(), runtime.SessionTitle("child", "Sub-agent task"))
	assert.Equal(t, []string{"docker agent: Fix auth bug"}, titles)
	assert.Equal(t, "Fix auth bug", m.sessionState.SessionTitle())

	// Updated title emits the new value.
	m.handleEvent(t.Context(), runtime.SessionTitle("root", "Add OAuth tests"))
	assert.Equal(t, []string{"docker agent: Fix auth bug", "docker agent: Add OAuth tests"}, titles)
}

func TestSessionTitleCustomAppName(t *testing.T) {
	t.Parallel()

	m := bareModel(24)
	m.appName = "custom agent"
	m.app = app.New(t.Context(), &cycleThinkingRuntime{}, session.New(session.WithID("root")))

	var titles []string
	m.setWindowTitle = func(title string) {
		titles = append(titles, title)
	}

	m.handleEvent(t.Context(), runtime.SessionTitle("root", "Debug build"))
	assert.Equal(t, []string{"custom agent: Debug build"}, titles)
}

func TestNewSessionResetsWindowTitleOnlyAfterSet(t *testing.T) {
	t.Parallel()

	m := bareModel(24)
	m.app = app.New(t.Context(), &cycleThinkingRuntime{}, session.New(session.WithID("root")))

	var titles []string
	m.setWindowTitle = func(title string) {
		titles = append(titles, title)
	}

	// /new before any title is received leaves the terminal title alone.
	assert.True(t, m.handleSlash(t.Context(), "/new", busySubmitSteer))
	assert.Empty(t, titles)

	// Once a title has been set, /new resets it to the app name.
	m.handleEvent(t.Context(), runtime.SessionTitle(m.app.Session().ID, "Investigate leak"))
	assert.True(t, m.handleSlash(t.Context(), "/new", busySubmitSteer))
	assert.Equal(t, []string{"docker agent: Investigate leak", "docker agent"}, titles)
	assert.Empty(t, m.sessionState.SessionTitle())
}

func TestResumeSessionUpdatesWindowTitle(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewInMemorySessionStore()

	titled := session.New(session.WithID("titled"), session.WithWorkingDir(dir), session.WithTitle("Refactor parser"))
	untitled := session.New(session.WithID("untitled"), session.WithWorkingDir(dir))
	require.NoError(t, store.AddSession(t.Context(), titled))
	require.NoError(t, store.AddSession(t.Context(), untitled))

	current := session.New(session.WithID("current"), session.WithWorkingDir(dir))
	m := bareModel(24)
	m.app = app.New(t.Context(), &cycleThinkingRuntime{store: store}, current)

	var titles []string
	m.setWindowTitle = func(title string) {
		titles = append(titles, title)
	}

	m.resumeSession(t.Context(), titled.ID)
	assert.Equal(t, []string{"docker agent: Refactor parser"}, titles)

	m.resumeSession(t.Context(), untitled.ID)
	assert.Equal(t, []string{"docker agent: Refactor parser", "docker agent"}, titles)
}

type stubTitleCompleter struct {
	title string
}

func (c stubTitleCompleter) Name() string { return "stub" }

func (c stubTitleCompleter) Complete(context.Context, string, string) (string, error) {
	return c.title, nil
}

func TestAppRunGeneratesSessionTitleForLeanTUI(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	sess := session.New(session.WithID("root"))
	gen := sessiontitle.NewFromCompleters(stubTitleCompleter{title: "Generated title"})
	a := app.New(ctx, &cycleThinkingRuntime{}, sess, app.WithTitleGenerator(gen))

	m := bareModel(24)
	m.app = a
	gotTitle := make(chan string, 1)
	m.setWindowTitle = func(title string) {
		select {
		case gotTitle <- title:
		default:
		}
	}

	events := make(chan any, 64)
	ready := make(chan struct{})
	go a.SubscribeReliable(ctx, func(msg tea.Msg) {
		select {
		case events <- msg:
		case <-ctx.Done():
		}
	}, app.WithSubscriptionReady(func() { close(ready) }), app.WithGenerationEventMapper(func(msg tea.Msg, generation uint64) tea.Msg {
		return leanEvent{generation: m.eventGeneration.Load(), inner: msg, valid: func() bool { return m.app.IsEventGeneration(generation) }}
	}))
	<-ready
	a.Start(ctx)

	m.startRun(ctx, "help me fix auth", nil)

	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-events:
			m.handleEvent(ctx, ev)
		case title := <-gotTitle:
			assert.Equal(t, "docker agent: Generated title", title)
			return
		case <-deadline:
			t.Fatal("timed out waiting for generated session title")
		}
	}
}
