package messages

import "github.com/docker/docker-agent/pkg/tools"

// MCP messages control MCP prompt interactions and elicitation.
type (
	// MCPPromptMsg executes an MCP prompt with arguments.
	MCPPromptMsg struct {
		PromptName string
		Arguments  map[string]string
	}

	// ShowMCPPromptInputMsg shows input dialog for MCP prompt.
	ShowMCPPromptInputMsg struct {
		PromptName string
		PromptInfo any // tools.PromptInfo
	}

	// ElicitationResponseMsg contains response to an elicitation request.
	ElicitationResponseMsg struct {
		Action  tools.ElicitationAction
		Content map[string]any
		// ElicitationID correlates this response with the specific request it
		// answers; empty for dialogs built before the ID was plumbed through
		// (falls back to the runtime's sole-pending-request behavior).
		ElicitationID string
	}
)
