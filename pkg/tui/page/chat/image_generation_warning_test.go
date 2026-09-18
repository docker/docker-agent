package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
)

func TestImageGenerationWarningUsesNotificationBanner(t *testing.T) {
	t.Parallel()

	p := newTestChatPage(t)
	const message = "The model returned text but no image for this image-generation request. Try rephrasing the request."

	handled, cmd := p.handleRuntimeEvent(runtime.Warning(message, "root"))
	require.True(t, handled)
	require.NotNil(t, cmd)

	shown, ok := cmd().(notification.ShowMsg)
	require.True(t, ok)
	assert.Equal(t, message, shown.Text)
	assert.Equal(t, notification.TypeWarning, shown.Type)
}
