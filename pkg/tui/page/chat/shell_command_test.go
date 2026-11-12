package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestBangCommandProgressAppearsInMessages(t *testing.T) {
	t.Parallel()
	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)
	t.Cleanup(func() { Cleanup(p) })
	p.SetSize(100, 30)

	handled, _ := p.handleRuntimeEvent(runtime.ShellCommandStarted("bang-1", "printf first; printf second"))
	require.True(t, handled)
	view := ansi.Strip(p.messages.View())
	assert.Contains(t, view, "printf first; printf second")

	handled, _ = p.handleRuntimeEvent(runtime.ShellCommandOutput("bang-1", "first"))
	require.True(t, handled)
	assert.Contains(t, ansi.Strip(p.messages.View()), "first")

	handled, _ = p.handleRuntimeEvent(runtime.ShellCommandFinished("bang-1", "printf first; printf second", "firstsecond", nil))
	require.True(t, handled)
	view = ansi.Strip(p.messages.View())
	assert.Contains(t, view, "firstsecond")
	assert.NotContains(t, strings.ToLower(view), "running")
}
