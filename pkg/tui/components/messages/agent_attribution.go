package messages

import (
	"slices"

	transfertask "github.com/docker/docker-agent/pkg/tools/builtin/transfertask/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentattribution"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Freeze a running transfer that introduced a visible reply at its original position.
func (m *model) keepInterruptedTransferCues() {
	var speaker string
	for i, msg := range slices.Backward(m.messages) {
		if msg.Type == types.MessageTypeUser || msg.Type == types.MessageTypeCancelled {
			return
		}
		if agentattribution.HasVisibleReply(msg) {
			speaker = msg.Sender
		}
		if speaker == "" || msg.Sender == speaker {
			continue
		}
		switch msg.Type {
		case types.MessageTypeAgentTransition:
			if msg.Content == speaker {
				// The source of this inferred switch may itself have been
				// introduced by an interrupted transfer.
				speaker = msg.Sender
			}
		case types.MessageTypeAgentReturn:
			if msg.Content == speaker {
				speaker = ""
			}
		case types.MessageTypeToolCall:
			if agentattribution.ControlTarget(msg) != speaker {
				continue
			}
			if msg.ToolCall.Function.Name == transfertask.ToolNameTransferTask {
				msg.HideToolProgress = true
				animation.StopView(m.views[i])
				m.invalidateItem(i)
			}
			speaker = msg.Sender
		}
	}
}
