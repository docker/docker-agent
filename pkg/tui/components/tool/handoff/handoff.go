package handoff

import (
	handoff "github.com/docker/docker-agent/pkg/tools/builtin/handoff/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func New(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	return toolcommon.NewBase(ar, msg, sessionState, render)
}

func render(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, _ int) string {
	params, err := toolcommon.ParseArgs[handoff.Args](msg.ToolCall.Function.Arguments)
	if err != nil || params.Agent == "" {
		hideResults := false
		if sessionState != nil {
			hideResults = sessionState.HideToolResults()
		}
		return toolcommon.RenderTool(msg, s, "", "", width, hideResults)
	}

	return styles.AgentBadgeStyleFor(msg.Sender).MarginLeft(2).Render(msg.Sender) + " ─► " + styles.AgentBadgeStyleFor(params.Agent).Render(params.Agent)
}
