package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestApp_NewSession_PreservesWorkingDir(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rt := &mockRuntime{}

	initialSess := session.New(
		session.WithWorkingDir("/projects/myapp"),
	)
	app := New(ctx, rt, initialSess)
	require.Equal(t, "/projects/myapp", app.Session().WorkingDir)

	app.NewSession()

	assert.Equal(t, "/projects/myapp", app.Session().WorkingDir,
		"NewSession must preserve WorkingDir so tools keep operating in the same directory")
	assert.Equal(t, []string{"/projects/myapp"}, app.Session().AllowedDirectories(),
		"AllowedDirectories must reflect the preserved WorkingDir")
}

func TestApp_NewSession_PreservesAllSessionFlags(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rt := &mockRuntime{}

	initialSess := session.New(
		session.WithToolsApproved(true),
		session.WithHideToolResults(true),
		session.WithWorkingDir("/work"),
	)
	app := New(ctx, rt, initialSess)

	app.NewSession()

	s := app.Session()
	assert.True(t, s.ToolsApproved)
	assert.True(t, s.HideToolResults)
	assert.Equal(t, "/work", s.WorkingDir)
}
