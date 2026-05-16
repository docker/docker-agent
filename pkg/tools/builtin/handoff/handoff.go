package handoff

import (
	"context"

	"github.com/docker/docker-agent/pkg/tools"
)

const ToolNameHandoff = "handoff"

type Tool struct{}

var _ tools.ToolSet = (*Tool)(nil)

type Args struct {
	Agent string `json:"agent" jsonschema:"The name of the agent to hand off the conversation to."`
}

func NewHandoffTool() *Tool {
	return &Tool{}
}

func (t *Tool) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:        ToolNameHandoff,
			Category:    "handoff",
			Description: "Use this function to hand off the conversation to the selected agent.",
			Parameters:  tools.MustSchemaFor[Args](),
			Annotations: tools.ToolAnnotations{
				ReadOnlyHint: true,
				Title:        "Handoff Conversation",
			},
		},
	}, nil
}
