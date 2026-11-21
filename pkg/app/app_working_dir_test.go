package app

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/session"
)

func TestApp_NewSession_PreservesSessionOptions(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{}

	initialSess := session.New(
		session.WithToolsApproved(true),
		session.WithHideToolResults(true),
		session.WithWorkingDir("/work"),
	)
	app := New(t.Context(), rt, initialSess)

	app.NewSession()

	s := app.Session()
	assert.True(t, s.ToolsApproved)
	assert.True(t, s.HideToolResults)
	assert.Equal(t, "/work", s.WorkingDir)
	assert.Equal(t, []string{"/work"}, s.AllowedDirectories())
}
