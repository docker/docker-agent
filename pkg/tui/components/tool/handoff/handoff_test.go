package handoff

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
	handofftool "github.com/docker/docker-agent/pkg/tools/builtin/handoff/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestHandoffFallsBackToToolHeaderWhenArgumentsCannotParse(t *testing.T) {
	t.Parallel()

	msg := types.ToolCallMessage("root", tools.ToolCall{
		ID: "call-1",
		Function: tools.FunctionCall{
			Name:      handofftool.ToolNameHandoff,
			Arguments: `{"agent":`,
		},
	}, tools.Tool{Name: handofftool.ToolNameHandoff}, types.ToolStatusPending)

	view := New(animation.NewRuntime(), msg, service.StaticSessionState{})
	_ = view.SetSize(80, 0)

	assert.Contains(t, ansi.Strip(view.View()), handofftool.ToolNameHandoff)
}
