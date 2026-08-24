package chat

import (
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestNestedStreamStopDoesNotFinalizeScrolledUpParentTail(t *testing.T) {
	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)
	p.messages.SetSize(60, 8)

	_, _ = p.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
	_, _ = p.handleRuntimeEvent(runtime.StreamStarted("child-session", "child"))
	require.Equal(t, 2, p.streamDepth)

	p.messages.AddUserMessage(strings.Repeat("history line\n", 40))
	p.messages.AddAssistantMessage("root", "")
	p.messages.AppendToLastMessage("root", "parent tail start\n")
	_ = p.messages.View()
	_, _ = p.messages.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	p.messages.AppendToLastMessage("root", "deferred parent marker\n")
	require.Positive(t, deferredTailLen(t, p), "fixture must be scrolled up with a deferred parent tail")

	_, _ = p.handleRuntimeEvent(runtime.StreamStopped("child-session", "child", "normal"))
	require.Equal(t, 1, p.streamDepth)
	require.Positive(t, deferredTailLen(t, p), "a nested stop must not finalize the still-active parent response")
	require.True(t, p.working)

	_, _ = p.handleRuntimeEvent(runtime.StreamStopped(sess.ID, "root", "normal"))
	require.Zero(t, p.streamDepth)
	require.Zero(t, deferredTailLen(t, p), "the outermost stop is the exact-content finalization boundary")
	require.False(t, p.working)
}

func deferredTailLen(t *testing.T, p *chatPage) int {
	t.Helper()
	value := reflect.ValueOf(p.messages)
	require.Equal(t, reflect.Pointer, value.Kind())
	field := value.Elem().FieldByName("deferredTail")
	require.True(t, field.IsValid(), "messages model must retain deferred-tail state")
	return field.Len()
}
