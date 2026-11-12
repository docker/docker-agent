package transfertask

import (
	"context"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask/types"
)

const ToolNameTransferTask = types.ToolNameTransferTask

type ToolSet struct{}

var (
	_ tools.ToolSet = (*ToolSet)(nil)
	_ tools.Named   = (*ToolSet)(nil)
)

type Args = types.Args

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
