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
		PromptInfo any // mcptools.PromptInfo but avoiding import cycles
	}

	// ElicitationResponseMsg contains response to an elicitation request.
	ElicitationResponseMsg struct {
		Action  tools.ElicitationAction
		Content map[string]any
	}
)
