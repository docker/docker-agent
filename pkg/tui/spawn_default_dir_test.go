package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
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

// /new <dir> must win over the configured default (#4046). Before the fix
// the /new command dropped its argument, so the default always won.
func TestNewSessionMsg_ExplicitDirOverridesDefault(t *testing.T) {
	t.Parallel()

	spy := &spySpawner{}
	m := newSpawnTestModel(t, spy, WithDefaultWorkingDir("/default/dir"))
	dir := t.TempDir()

	_, _ = m.Update(messages.NewSessionMsg{WorkingDir: dir})

	assert.Equal(t, []string{dir}, spy.dirs,
		"an explicit /new directory must win over the configured default")
	assert.Equal(t, 2, m.supervisor.Count(),
		"the new session must be added instead of opening the picker")
}

// A relative /new argument resolves against the active session's working
// directory, not the process CWD.
func TestNewSessionMsg_RelativeDirResolvesFromActiveSession(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	sub := filepath.Join(base, "sub")
	require.NoError(t, os.Mkdir(sub, 0o755))

	spy := &spySpawner{}
	m := newSpawnTestModel(t, spy)
	// Point the active session at base; the helper registers it under
	// "/initial", which does not exist on disk.
	m.supervisor.GetRunner(m.supervisor.ActiveID()).WorkingDir = base

	_, _ = m.Update(messages.NewSessionMsg{WorkingDir: "sub"})

	assert.Equal(t, []string{sub}, spy.dirs,
		"a relative directory must resolve from the active session's working dir")
}

// ~ and environment variables in an explicit /new directory are expanded via
// path.ExpandPath. HOME is overridden (ExpandHomeDir prefers it over the OS
// account lookup) so the test stays hermetic; t.Setenv forbids t.Parallel.
func TestNewSessionMsg_ExpandsTildeAndEnv(t *testing.T) {
	base := t.TempDir()
	sub := filepath.Join(base, "sub")
	require.NoError(t, os.Mkdir(sub, 0o755))
	t.Setenv("HOME", base)
	t.Setenv("CAGENT_TEST_NEW_DIR", base)

	spy := &spySpawner{}
	m := newSpawnTestModel(t, spy)

	_, _ = m.Update(messages.NewSessionMsg{WorkingDir: "~/sub"})
	_, _ = m.Update(messages.NewSessionMsg{WorkingDir: "$CAGENT_TEST_NEW_DIR/sub"})

	assert.Equal(t, []string{sub, sub}, spy.dirs,
		"~ and environment variables must be expanded")
}

// A /new directory that does not exist must not reach the spawner; the user
// gets an error notification instead.
func TestNewSessionMsg_MissingDirErrorsWithoutSpawning(t *testing.T) {
	t.Parallel()

	spy := &spySpawner{}
	m := newSpawnTestModel(t, spy, WithDefaultWorkingDir("/default/dir"))
	missing := filepath.Join(t.TempDir(), "missing")

	_, cmd := m.Update(messages.NewSessionMsg{WorkingDir: missing})

	assert.Empty(t, spy.dirs, "the spawner must not run for a missing directory")
	assert.Equal(t, 1, m.supervisor.Count(), "no session must be added")
	note, ok := firstOfType[notification.ShowMsg](collectMsgs(cmd))
	require.True(t, ok, "an error notification must be shown")
	assert.Equal(t, notification.TypeError, note.Type)
	assert.Contains(t, note.Text, missing)
}

// A /new argument pointing at a file must not reach the spawner either.
func TestNewSessionMsg_FileArgErrorsWithoutSpawning(t *testing.T) {
	t.Parallel()

	spy := &spySpawner{}
	m := newSpawnTestModel(t, spy)
	file := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))

	_, cmd := m.Update(messages.NewSessionMsg{WorkingDir: file})

	assert.Empty(t, spy.dirs, "the spawner must not run for a non-directory path")
	note, ok := firstOfType[notification.ShowMsg](collectMsgs(cmd))
	require.True(t, ok, "an error notification must be shown")
	assert.Equal(t, notification.TypeError, note.Type)
	assert.Contains(t, note.Text, "is not a directory")
}
