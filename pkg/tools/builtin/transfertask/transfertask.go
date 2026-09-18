package transfertask

import (
	"context"

	"github.com/docker/docker-agent/pkg/tools"
)

const ToolNameTransferTask = "transfer_task"

type ToolSet struct{}

var (
	_ tools.ToolSet = (*ToolSet)(nil)
	_ tools.Named   = (*ToolSet)(nil)
)

type Args struct {
	Agent          string `json:"agent" jsonschema:"The name of the agent to transfer the task to."`
	Task           string `json:"task" jsonschema:"A clear and concise description of the task the member should achieve."`
	ExpectedOutput string `json:"expected_output" jsonschema:"The expected output from the member (optional)."`
}

func New() *ToolSet {
	return &ToolSet{}
}

// Name implements tools.Named; loader-created, so no registry WithName wrapper.
func (t *ToolSet) Name() string {
	return ToolNameTransferTask
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:           ToolNameTransferTask,
			RuntimeHandler: ToolNameTransferTask,
			Category:       "transfer",
			Description: `Use this function to transfer a task to the selected team member.
            You must provide a clear and concise description of the task the member should achieve AND the expected output.`,
			Parameters: tools.MustSchemaFor[Args](),
			Annotations: tools.ToolAnnotations{
				ReadOnlyHint: true,
				Title:        "Transfer Task",
			},
		},
	}, nil
}
