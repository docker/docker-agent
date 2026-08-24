package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/statusbar"
	"github.com/docker/docker-agent/pkg/tui/components/tabbar"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// spySpawner records the working directories it was asked to spawn in and
// returns a live app wired to stubRuntime so the full spawn path runs.
type spySpawner struct {
	dirs []string
}

func (s *spySpawner) spawn(ctx context.Context, workingDir string) (*app.App, *session.Session, func(), error) {
	s.dirs = append(s.dirs, workingDir)
	sess := session.New(session.WithWorkingDir(workingDir))
	return app.New(ctx, stubRuntime{}, sess), sess, func() {}, nil
}

// newSpawnTestModel wires a model with a real supervisor around spy plus the
// components handleSpawnSession/handleSwitchTab touch on a successful spawn.
func newSpawnTestModel(t *testing.T, spy *spySpawner, opts ...Option) *appModel {
	t.Helper()
	m, _ := newTestModel(t)
	m.ar = animation.NewRuntime()
	m.buildCommandCategories = func(context.Context, tea.Model) []commands.Category { return nil }
	m.application = app.New(t.Context(), stubRuntime{}, session.New())
	m.workingSpinner = spinner.New(m.ar, spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle)
	m.tabBar = tabbar.New(m.ar, 0)
	m.statusBar = statusbar.New(m)
	m.supervisor = supervisor.New(spy.spawn)
	// Mirror New: the initial session is registered with the supervisor.
	m.supervisor.AddSession(t.Context(), m.application, m.application.Session(), "/initial", func() {})
	m.width, m.height = 120, 40
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// A generic spawn request (no directory) must reuse the configured default
// working directory instead of opening the picker (#4039).
func TestHandleSpawnSession_EmptyDirUsesConfiguredDefault(t *testing.T) {
	t.Parallel()

	spy := &spySpawner{}
	m := newSpawnTestModel(t, spy, WithDefaultWorkingDir("/work/dir"))

	_, _ = m.handleSpawnSession("")

	assert.Equal(t, []string{"/work/dir"}, spy.dirs,
		"the spawner must receive the configured default directory")
	assert.Equal(t, 2, m.supervisor.Count(),
		"the new session must be added instead of opening the picker")
}

// Without a configured default the picker keeps opening, exactly as before.
func TestHandleSpawnSession_EmptyDirWithoutDefaultOpensPicker(t *testing.T) {
	t.Parallel()

	spy := &spySpawner{}
	m := newSpawnTestModel(t, spy)

	_, cmd := m.handleSpawnSession("")

	assert.Empty(t, spy.dirs, "nothing must be spawned without a directory")
	require.NotNil(t, cmd)
	assert.True(t, hasMsg[dialog.OpenDialogMsg](collectMsgs(cmd)),
		"the working-directory picker must open")
}

// A concrete directory in the request (picker selection, explicit
// SpawnSessionMsg) wins over the configured default.
func TestHandleSpawnSession_ExplicitDirOverridesDefault(t *testing.T) {
	t.Parallel()

	spy := &spySpawner{}
	m := newSpawnTestModel(t, spy, WithDefaultWorkingDir("/default/dir"))

	_, _ = m.handleSpawnSession("/explicit/dir")

	assert.Equal(t, []string{"/explicit/dir"}, spy.dirs,
		"an explicit directory must win over the configured default")
}

// The /new command routes through the same handler: with a configured
// default it must spawn there instead of opening the picker.
func TestNewSessionMsg_UsesConfiguredDefaultDir(t *testing.T) {
	t.Parallel()

	spy := &spySpawner{}
	m := newSpawnTestModel(t, spy, WithDefaultWorkingDir("/work/dir"))

	_, _ = m.Update(messages.NewSessionMsg{})

	assert.Equal(t, []string{"/work/dir"}, spy.dirs,
		"/new must spawn in the configured default directory")
	assert.Equal(t, 2, m.supervisor.Count(),
		"the new session must be added instead of opening the picker")
}
