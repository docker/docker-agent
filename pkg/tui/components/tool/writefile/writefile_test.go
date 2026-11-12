package writefile

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestStreamingArgumentsKeepLastPathWhenJSONTemporarilyInvalid(t *testing.T) {
	t.Parallel()

	msg := types.ToolCallMessage("agent", tools.ToolCall{
		ID: "call-1",
		Function: tools.FunctionCall{
			Name:      filesystem.ToolNameWriteFile,
			Arguments: `{"path": "/tmp/file"`,
		},
	}, tools.Tool{Name: filesystem.ToolNameWriteFile}, types.ToolStatusPending)

	view := New(animation.NewRuntime(), msg, service.StaticSessionState{})
	_ = view.SetSize(80, 0)

	first := ansi.Strip(view.View())
	require.Contains(t, first, "/tmp/file")

	msg.ToolCall.Function.Arguments += ","
	next := ansi.Strip(view.View())
	assert.Contains(t, next, "/tmp/file")
}
