// Package agentattribution infers speaker transitions for chat transcripts.
package agentattribution

import (
	"slices"
	"strings"

	handoff "github.com/docker/docker-agent/pkg/tools/builtin/handoff/types"
	transfertask "github.com/docker/docker-agent/pkg/tools/builtin/transfertask/types"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// IsTransitionTool reports whether the tool has a dedicated agent transition row.
func IsTransitionTool(name string) bool {
	return name == handoff.ToolNameHandoff || name == transfertask.ToolNameTransferTask
}

// HasVisibleReply reports whether the message carries visible assistant output.
func HasVisibleReply(msg *types.Message) bool {
	switch msg.Type {
	case types.MessageTypeAssistant:
		return strings.TrimSpace(msg.Content) != "" || len(msg.AssistantMedia) != 0
	case types.MessageTypeAssistantReasoningBlock:
		return msg.Content != ""
	default:
		return false
	}
}

// Boundary returns an inferred cue when the preceding rows do not explain the next speaker.
func Boundary(history []*types.Message, agentName string) *types.Message {
	if agentName == "" {
		return nil
	}
	// Start at the last visible speaker, or at the beginning of this turn if
	// nobody has spoken yet. Follow only connected tool transitions; ordinary
	// tools and unconnected calls from interleaved sessions don't take control.
	var owner string
	start := len(history)
	for i, msg := range slices.Backward(history) {
		if msg.Type == types.MessageTypeUser || msg.Type == types.MessageTypeCancelled {
			break
		}
		if HasVisibleReply(msg) {
			owner = msg.Sender
			break
		}
		start = i
	}
	for _, msg := range history[start:] {
		switch msg.Type {
		case types.MessageTypeAgentReturn, types.MessageTypeAgentTransition:
			// The rendered switch is authoritative, even if a delegate
			// handed off to another speaker before its return to the caller.
			owner = msg.Content
		case types.MessageTypeToolCall:
			to := agentToolTarget(msg)
			if to == "" || msg.Sender == "" || (owner != "" && owner != msg.Sender) {
				continue
			}
			// The named caller can introduce a tool-only turn. Only a
			// successful handoff or an active delegation moves control:
			// a finished transfer_task (also on reload) is back at its caller.
			owner = msg.Sender
			if toolMovesControl(msg) {
				owner = to
			}
		}
	}
	if owner != "" && owner != agentName {
		return types.Agent(types.MessageTypeAgentTransition, owner, agentName)
	}
	return nil
}

func agentToolTarget(msg *types.Message) string {
	call := msg.ToolCall.Function
	if IsTransitionTool(call.Name) {
		args, err := toolcommon.ParseArgs[handoff.Args](call.Arguments)
		if err == nil {
			return args.Agent
		}
	}
	return ""
}

func toolMovesControl(msg *types.Message) bool {
	switch msg.ToolCall.Function.Name {
	case handoff.ToolNameHandoff:
		return msg.ToolStatus == types.ToolStatusCompleted
	case transfertask.ToolNameTransferTask:
		return msg.ToolStatus == types.ToolStatusRunning
	default:
		return false
	}
}

// ControlTarget returns the recipient of a successful handoff or live delegation.
func ControlTarget(msg *types.Message) string {
	if toolMovesControl(msg) {
		return agentToolTarget(msg)
	}
	return ""
}
