// Package codemode renders the JavaScript script without its orchestration output.
package codemode

import (
	"encoding/json"
	"strings"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type scriptArgs struct {
	Script string `json:"script"`
}

func New(ar *animation.Runtime, msg *types.Message, state service.SessionStateReader) layout.Model {
	return toolcommon.NewBase(ar, msg, state, render)
}

func render(msg *types.Message, s spinner.Spinner, state service.SessionStateReader, width, _ int) string {
	header := toolcommon.RenderTool(msg, s, "", "", width, state.HideToolResults())
	var args scriptArgs
	if err := json.Unmarshal([]byte(msg.ToolCall.Function.Arguments), &args); err != nil || args.Script == "" {
		return header
	}
	script := markdown.NewFastRenderer(max(width, 1)).RenderCodeBlock(args.Script, "javascript")
	return header + "\n" + strings.TrimRight(script, "\n")
}
