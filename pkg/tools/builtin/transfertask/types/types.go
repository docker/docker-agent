// Package types defines the shared wire contracts for the transfertask tools.
package types

const ToolNameTransferTask = "transfer_task"

type Args struct {
	Agent          string `json:"agent" jsonschema:"The name of the agent to transfer the task to."`
	Task           string `json:"task" jsonschema:"A clear and concise description of the task the member should achieve."`
	ExpectedOutput string `json:"expected_output" jsonschema:"The expected output from the member (optional)."`
}
