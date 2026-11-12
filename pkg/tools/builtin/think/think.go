package think

import (
	"context"
	"strings"

	"github.com/docker/docker-agent/pkg/tools"
)

const ToolNameThink = "think"

// CreateToolSet is used by the tools registry.
func CreateToolSet() (tools.ToolSet, error) {
	return New(), nil
}

// ToolSet has nonzero size so distinct instances have distinct pointer identities.
type ToolSet struct{ _ byte }

type Args struct {
	Thought string `json:"thought" jsonschema:"The thought to think about"`
}

func (t *ToolSet) callTool(ctx context.Context, params Args, rt tools.Runtime) (*tools.ToolCallResult, error) {
	thoughts := []string{params.Thought}
	if appender, ok := rt.(tools.ScratchpadAppender); ok {
		var err error
		thoughts, err = appender.AppendThought(ctx, t, params.Thought)
		if err != nil {
			return tools.ResultError(err.Error()), nil
		}
	} else if _, unscoped := rt.(tools.NopRuntime); !unscoped {
		return tools.ResultError("think scratchpad unavailable in this runtime"), nil
	}
	return tools.ResultSuccess("Thoughts:\n" + strings.Join(thoughts, "\n")), nil
}

func New() *ToolSet {
	return &ToolSet{}
}

func (t *ToolSet) Instructions() string {
	return `## Think Tool

Use the think tool as a scratchpad before acting. Think to:
- Check which rules or policies apply
- Verify you have all required information
- Validate planned actions before executing
- Reason through complex multi-step problems`
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:         ToolNameThink,
			Category:     "think",
			Description:  "Use the tool to think about something. It will not obtain new information or change external data; it appends to an internal conversation scratchpad. Use it when complex reasoning or some cache memory is needed.",
			Parameters:   tools.MustSchemaFor[Args](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewRuntimeHandler(t.callTool),
			Annotations: tools.ToolAnnotations{
				ReadOnlyHint: true,
				Title:        "Think",
			},
		},
	}, nil
}
