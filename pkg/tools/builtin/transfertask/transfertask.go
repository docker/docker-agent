package transfertask

import (
	"context"

	"github.com/docker/docker-agent/pkg/tools"
)

const ToolNameTransferTask = "transfer_task"

type Tool struct{}

var _ tools.ToolSet = (*Tool)(nil)

type Args struct {
	Agent          string `json:"agent" jsonschema:"The name of the agent to transfer the task to."`
	Task           string `json:"task" jsonschema:"A clear and concise description of the task the member should achieve."`
	ExpectedOutput string `json:"expected_output" jsonschema:"The expected output from the member (optional)."`
}

func NewTransferTaskTool() *Tool {
	return &Tool{}
}

func (t *Tool) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:     ToolNameTransferTask,
			Category: "transfer",
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
