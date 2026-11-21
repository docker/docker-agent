package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// TestWarningRoutingIsSessionScoped pins the warning routing contract: a
// warning tagged with the root session shows as the root's own notice, while
// one tagged with a sub-session is labelled with its agent so it is not
// presented as the root chat's notice. Untagged warnings are unchanged.
func TestWarningRoutingIsSessionScoped(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)

	notice := func(t *testing.T, ev runtime.Event) string {
		t.Helper()
		handled, cmd := p.handleRuntimeEvent(ev)
		require.True(t, handled)
		require.NotNil(t, cmd)
		show, ok := cmd().(notification.ShowMsg)
		require.True(t, ok, "warning must surface as a notification")
		return show.Text
	}

	assert.Equal(t, "root warning",
		notice(t, runtime.WarningForSession(sess.ID, "root warning", "root")),
		"a warning for the root session is the root's own notice")
	assert.Equal(t, "worker: sub warning",
		notice(t, runtime.WarningForSession("sub-session-1", "sub warning", "worker")),
		"a sub-session warning must be labelled with its agent, not shown as the root's notice")
	assert.Equal(t, "untagged warning",
		notice(t, runtime.Warning("untagged warning", "root")),
		"untagged warnings keep the existing presentation")
}
