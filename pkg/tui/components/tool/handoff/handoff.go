package handoff

import (
	"encoding/json"

	"github.com/docker/docker-agent/pkg/tools/builtin/handoff"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func New(msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	return toolcommon.NewBase(msg, sessionState, render)
}

func render(msg *types.Message, _ spinner.Spinner, _ service.SessionStateReader, _, _ int) string {
	var params handoff.Args
	if err := json.Unmarshal([]byte(msg.ToolCall.Function.Arguments), &params); err != nil {
		return ""
	}

	return styles.AgentBadgeStyleFor(msg.Sender).MarginLeft(2).Render(msg.Sender) + " ─► " + styles.AgentBadgeStyleFor(params.Agent).Render(params.Agent)
}
