package think

import (
	"context"
	"strings"

	"github.com/docker/docker-agent/pkg/tools"
)

const ToolNameThink = "think"

type Tool struct {
	thoughts []string
}

// Verify interface compliance
var (
	_ tools.ToolSet      = (*Tool)(nil)
	_ tools.Instructable = (*Tool)(nil)
)

type Args struct {
	Thought string `json:"thought" jsonschema:"The thought to think about"`
}

func (t *Tool) callTool(_ context.Context, params Args) (*tools.ToolCallResult, error) {
	t.thoughts = append(t.thoughts, params.Thought)
	return tools.ResultSuccess("Thoughts:\n" + strings.Join(t.thoughts, "\n")), nil
}

func NewThinkTool() *Tool {
	return &Tool{}
}

func (t *Tool) Instructions() string {
	return `## Think Tool

Use the think tool as a scratchpad before acting. Think to:
- Check which rules or policies apply
- Verify you have all required information
- Validate planned actions before executing
- Reason through complex multi-step problems`
}

func (t *Tool) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:         ToolNameThink,
			Category:     "think",
			Description:  "Use the tool to think about something. It will not obtain new information or change the database, but just append the thought to the log. Use it when complex reasoning or some cache memory is needed.",
			Parameters:   tools.MustSchemaFor[Args](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.callTool),
			Annotations: tools.ToolAnnotations{
				ReadOnlyHint: true,
				Title:        "Think",
			},
		},
	}, nil
}
