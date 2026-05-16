package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ElicitationAction string

const (
	ElicitationActionAccept  ElicitationAction = "accept"
	ElicitationActionDecline ElicitationAction = "decline"
	ElicitationActionCancel  ElicitationAction = "cancel"
)

// ElicitationHandler is a function type that handles elicitation requests from the MCP server
// This allows the runtime to handle elicitation requests and propagate them to its own client
type ElicitationHandler func(ctx context.Context, req *mcp.ElicitParams) (ElicitationResult, error)

type ElicitationResult struct {
	Action  ElicitationAction `json:"action"`
	Content map[string]any    `json:"content,omitempty"`
}
