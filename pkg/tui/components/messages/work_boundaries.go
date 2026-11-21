package messages

import (
	"encoding/json"
	"slices"

	"github.com/docker/docker-agent/pkg/tools"
	handoff "github.com/docker/docker-agent/pkg/tools/builtin/handoff/types"
	transfertask "github.com/docker/docker-agent/pkg/tools/builtin/transfertask/types"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// workAgent identifies activity that can establish the controlling sender. Tool
// completions update the original row and never establish a new sender.
func workAgent(msg *types.Message) string {
	switch msg.Type {
	case types.MessageTypeAssistant, types.MessageTypeAssistantReasoningBlock, types.MessageTypeToolCall:
		return msg.Sender
	}
	return ""
}

func (m *model) workSender() string {
	for _, msg := range slices.Backward(m.messages) {
		switch msg.Type {
		case types.MessageTypeUser:
			return ""
		case types.MessageTypeAgentHandoff, types.MessageTypeAgentReturn, types.MessageTypeAgentDelegation:
			return msg.Content
		}
		if !m.ordinaryOnly[msg] {
			if sender := workAgent(msg); sender != "" {
				return sender
			}
		}
	}
	return ""
}

func (m *model) noteWorkSender(sender string) {
	if sender == "" {
		return
	}
	if previous := m.workSender(); previous != "" && previous != sender {
		m.addWorkTransition(types.MessageTypeAgentHandoff, previous, sender)
	}
}

// AddAgentDelegation records an observed outbound transfer independently of
// the tool response, which arrives only after the child has returned.
func (m *model) AddAgentDelegation(from, to string) {
	m.addWorkTransition(types.MessageTypeAgentDelegation, from, to)
}

func (m *model) addWorkTransition(typ types.MessageType, from, to string) {
	if from == "" || to == "" || from == to {
		return
	}
	for _, last := range slices.Backward(m.messages) {
		if last.Type == types.MessageTypeSpinner {
			continue
		}
		if last.Type == typ && last.Sender == from && last.Content == to {
			return
		}
		break
	}
	if m.work.enabled {
		m.finishReasoningPreview(from, "", "")
	}
	// Compact-only rows do not change the ordinary previous-message state,
	// selection or finalization.
	msg := types.Agent(typ, from, to)
	idx := len(m.messages)
	if idx > 0 && m.messages[idx-1].Type == types.MessageTypeSpinner {
		idx--
	}
	m.messages = slices.Insert(m.messages, idx, msg)
	m.views = slices.Insert(m.views, idx, m.createMessageView(msg))
	m.renderDirty = true
	m.invalidateView()
}

func workTransitionForTool(call tools.ToolCall) (types.MessageType, string) {
	var args handoff.Args // Both tools use the same target field.
	var typ types.MessageType
	switch call.Function.Name {
	case handoff.ToolNameHandoff:
		typ = types.MessageTypeAgentHandoff
	case transfertask.ToolNameTransferTask:
		typ = types.MessageTypeAgentDelegation
	default:
		return 0, ""
	}
	if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil {
		return 0, ""
	}
	return typ, args.Agent
}

func compactOnly(msg *types.Message) bool {
	return msg.Type == types.MessageTypeAgentHandoff || msg.Type == types.MessageTypeAgentDelegation || msg.CompactOnly
}

func (m *model) hiddenWorkSource(index int) bool {
	if m.work.enabled {
		return m.ordinaryOnly[m.messages[index]]
	}
	return compactOnly(m.messages[index])
}

func (m *model) nextOrdinarySource(index int) int {
	for i := index + 1; i < len(m.messages); i++ {
		if !compactOnly(m.messages[i]) {
			return i
		}
	}
	return len(m.messages)
}

func (m *model) lastOrdinarySource() int {
	for i, msg := range slices.Backward(m.messages) {
		if !compactOnly(msg) {
			return i
		}
	}
	return -1
}

func (m *model) noteWorkToolResult(call *types.Message, result *tools.ToolCallResult, wasCompleted bool) {
	if wasCompleted || call.ToolStatus != types.ToolStatusCompleted || result == nil || result.IsError {
		return
	}
	if typ, target := workTransitionForTool(call.ToolCall); typ == types.MessageTypeAgentHandoff {
		m.addWorkTransition(typ, call.Sender, target)
	}
}

// splitReplayReasoning separates the compact rendering at a transfer whose
// child transcript is absent. The original reasoning card still accumulates
// the parent's full saved reasoning for the ordinary transcript.
func (m *model) splitReplayReasoning(index int, block *reasoningblock.Model) (ordinary, compact *reasoningblock.Model) {
	source := m.messages[index]
	if m.ordinaryOnly == nil {
		m.ordinaryOnly = make(map[*types.Message]bool)
	}
	if !m.ordinaryOnly[source] {
		m.ordinaryOnly[source] = true
		part := *source
		part.CompactOnly = true
		m.messages = slices.Insert(m.messages, index+1, &part)
		m.views = slices.Insert[[]layout.Model, layout.Model](m.views, index+1, block.ReplayFragment())
	}
	compact = reasoningblock.New(m.ar, nextBlockID(), source.Sender, m.sessionState, reasoningblock.WithToolRenderers(m.toolRenderers))
	part := types.Agent(types.MessageTypeAssistantReasoningBlock, source.Sender, "")
	part.CompactOnly = true
	m.messages = append(m.messages, part)
	m.views = append(m.views, compact)
	return block, compact
}

func addReplayReasoningTool(block *reasoningblock.Model, sender string, call tools.ToolCall, def tools.Tool, result *tools.ToolCallResult) {
	status := types.ToolStatusCompleted
	if result != nil && result.IsError {
		status = types.ToolStatusError
	}
	block.AddToolCall(types.ToolCallMessage(sender, call, def, status))
	if result != nil {
		block.UpdateToolResult(call.ID, result.Output, status, result)
	}
}
