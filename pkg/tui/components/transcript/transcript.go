// Package transcript provides an embeddable chat transcript: an ordered
// list of messages, each rendered by docker-agent's own message and tool
// components, stitched together with the same grouping rules as the full
// message list.
//
// It is the reusable subset of pkg/tui/components/messages for hosts that
// bring their own scrolling, selection, and dialog framework (e.g. the
// Gordon assistant embedded in the Docker Sandboxes TUI): no scrollbar, no
// mouse selection, no inline edits, no session/service coupling — just the
// message store, the per-message views, and their lifecycle (streaming
// appends, tool status updates, theme rebuilds, animation cleanup).
package transcript

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tools"
	transfertask "github.com/docker/docker-agent/pkg/tools/builtin/transfertask/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/message"
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Transcript is the chat history. The zero value is not usable; create one
// with New. It is not safe for concurrent use: like every Bubble Tea model,
// it must only be touched from the program's update loop.
type Transcript struct {
	toolRenderers *tool.Registry
	ar            *animation.Runtime
	state         service.SessionStateReader
	msgs          []*types.Message
	views         []layout.Model
	width         int
}

type Option func(*Transcript)

func WithToolRenderers(registry *tool.Registry) Option {
	return func(t *Transcript) {
		if registry != nil {
			t.toolRenderers = registry
		}
	}
}

// New creates an empty transcript. The session state is consulted by the
// tool views for rendering preferences; embedders without one should pass
// a service.StaticSessionState.
func New(ar *animation.Runtime, state service.SessionStateReader, opts ...Option) *Transcript {
	t := &Transcript{ar: ar, state: state, width: 80, toolRenderers: tool.NewRegistry()}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Append adds a message and returns the new view's Init command (spinner
// and running-tool views use it to start the animation tick stream).
func (t *Transcript) Append(msg *types.Message) tea.Cmd {
	v := t.newView(msg, t.lastMsg())
	t.msgs = append(t.msgs, msg)
	t.views = append(t.views, v)
	return v.Init()
}

// newView creates the right view for a message, like the message list's
// createToolCallView / createMessageView helpers.
func (t *Transcript) newView(msg, prev *types.Message) layout.Model {
	var v layout.Model
	if msg.Type == types.MessageTypeToolCall {
		v = t.toolRenderers.New(t.ar, msg, t.state)
	} else {
		v = message.New(t.ar, msg, prev)
	}
	_ = v.SetSize(t.width, 0)
	return v
}

func (t *Transcript) lastMsg() *types.Message {
	if n := len(t.msgs); n > 0 {
		return t.msgs[n-1]
	}
	return nil
}

// AppendToLastMessage grows the most recent message with streamed reply
// text when it is an assistant message from the given agent, or appends a
// fresh assistant message otherwise (including into an empty transcript,
// where the full message list would drop the text instead). A trailing
// waiting spinner is replaced. The message view re-renders incrementally:
// its internal markdown renderer only re-parses the trailing block.
func (t *Transcript) AppendToLastMessage(agentName, content string) tea.Cmd {
	t.RemoveLast(types.MessageTypeSpinner)
	if n := len(t.msgs); n > 0 {
		if last := t.msgs[n-1]; last.Type == types.MessageTypeAssistant && last.Sender == agentName {
			last.Content += content
			if v, ok := t.views[n-1].(message.Model); ok {
				return v.SetMessage(last)
			}
			return nil
		}
	}
	return t.Append(types.Agent(types.MessageTypeAssistant, agentName, content))
}

// AddOrUpdateToolCall surfaces a tool call: an existing entry with the same
// call ID has its status and streamed arguments updated, otherwise a new
// entry replaces any trailing waiting spinner. The same semantics as the
// message list's AddOrUpdateToolCall, without reasoning blocks.
func (t *Transcript) AddOrUpdateToolCall(agentName string, toolCall tools.ToolCall, toolDef tools.Tool, status types.ToolStatus) tea.Cmd {
	for i := range slices.Backward(t.msgs) {
		msg := t.msgs[i]
		if msg.Type != types.MessageTypeToolCall || msg.ToolCall.ID != toolCall.ID {
			continue
		}
		// Streamed argument deltas accumulate while the call is still
		// pending; later events carry the full arguments and replace them.
		if toolCall.Function.Arguments != "" {
			if status == types.ToolStatusPending {
				msg.ToolCall.Function.Arguments += toolCall.Function.Arguments
			} else {
				msg.ToolCall.Function.Arguments = toolCall.Function.Arguments
			}
		}
		if msg.ToolStatus == status {
			return nil
		}
		return t.refreshToolView(i, status)
	}
	t.RemoveLast(types.MessageTypeSpinner)
	return t.Append(types.ToolCallMessage(agentName, toolCall, toolDef, status))
}

// SetToolStatus updates the status of the tool call with the given ID,
// reporting whether an entry was found. The view is recreated so no
// memoized rendering of the previous status can survive the update.
func (t *Transcript) SetToolStatus(callID string, status types.ToolStatus) (tea.Cmd, bool) {
	for i := range slices.Backward(t.msgs) {
		if t.msgs[i].Type != types.MessageTypeToolCall || t.msgs[i].ToolCall.ID != callID {
			continue
		}
		return t.refreshToolView(i, status), true
	}
	return nil, false
}

// FinalizeToolCalls flips any tool entry still pending, running, or waiting
// for confirmation to the given terminal status. Defensive: a stream error
// (or a missed response event) must not leave an entry spinning forever.
func (t *Transcript) FinalizeToolCalls(status types.ToolStatus) {
	for i, msg := range t.msgs {
		if msg.Type != types.MessageTypeToolCall {
			continue
		}
		switch msg.ToolStatus {
		case types.ToolStatusPending, types.ToolStatusRunning, types.ToolStatusConfirmation:
			_ = t.refreshToolView(i, status)
		}
	}
}

// refreshToolView applies a status to tool message i and recreates its view.
func (t *Transcript) refreshToolView(i int, status types.ToolStatus) tea.Cmd {
	msg := t.msgs[i]
	msg.SetToolStatus(status)
	animation.StopView(t.views[i])
	var prev *types.Message
	if i > 0 {
		prev = t.msgs[i-1]
	}
	t.views[i] = t.newView(msg, prev)
	return t.views[i].Init()
}

// LastIs reports whether the most recent message has the given type.
func (t *Transcript) LastIs(typ types.MessageType) bool {
	n := len(t.msgs)
	return n > 0 && t.msgs[n-1].Type == typ
}

// RemoveLast drops the most recent message when it has the given type,
// stopping its animation subscription (mirrors the message list's
// RemoveSpinner contract: leaked subscriptions keep the tick stream alive).
func (t *Transcript) RemoveLast(typ types.MessageType) {
	n := len(t.msgs)
	if n == 0 || t.msgs[n-1].Type != typ {
		return
	}
	animation.StopView(t.views[n-1])
	t.msgs, t.views = t.msgs[:n-1], t.views[:n-1]
}

// Update forwards a message (animation ticks) to all views.
func (t *Transcript) Update(msg tea.Msg) tea.Cmd {
	var cmds []tea.Cmd
	for i, v := range t.views {
		u, cmd := v.Update(msg)
		t.views[i] = u
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

// Render renders the whole transcript at the given width, stitching the
// views together the way the message list does: each view contributes its
// lines, with one blank separator line between messages — except between
// consecutive tool calls, which group tightly.
func (t *Transcript) Render(width int) string {
	t.setWidth(width)
	var lines []string
	for i, v := range t.views {
		rendered := v.View()
		if rendered == "" {
			continue
		}
		if len(lines) > 0 && !t.groupsWithPrevious(i) {
			lines = append(lines, "")
		}
		lines = append(lines, strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")...)
	}
	return strings.Join(lines, "\n")
}

// groupsWithPrevious reports whether message i renders tightly under the
// previous one (consecutive tool calls), mirroring the message list's
// needsSeparator: transfer_task calls always get a separator so handoffs
// stand out.
func (t *Transcript) groupsWithPrevious(i int) bool {
	return i > 0 &&
		t.msgs[i].Type == types.MessageTypeToolCall &&
		t.msgs[i-1].Type == types.MessageTypeToolCall &&
		t.msgs[i].ToolCall.Function.Name != transfertask.ToolNameTransferTask
}

func (t *Transcript) setWidth(width int) {
	if width == t.width {
		return
	}
	t.width = width
	for _, v := range t.views {
		_ = v.SetSize(width, 0)
	}
}

// Rebuild recreates every view from its message, dropping all cached
// rendered output. Use it on theme change: the views memoize rendered
// ANSI, so a style swap must start from scratch. Returns the new views'
// Init commands (re-arming spinner animations).
func (t *Transcript) Rebuild() tea.Cmd {
	t.StopAnimations()
	var cmds []tea.Cmd
	var prev *types.Message
	for i, msg := range t.msgs {
		v := t.newView(msg, prev)
		t.views[i] = v
		if cmd := v.Init(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		prev = msg
	}
	return tea.Batch(cmds...)
}

// StopAnimations unregisters every view from the animation coordinator.
// Call it when the host view goes away (e.g. the embedding dialog closes)
// so abandoned spinners do not keep the tick stream alive; Rebuild re-arms
// them on the next open.
func (t *Transcript) StopAnimations() {
	for _, v := range t.views {
		animation.StopView(v)
	}
}

// Messages returns the transcript's messages, oldest first. The slice is
// the transcript's own backing store: callers must treat it as read-only
// (mutating entries would desync them from their rendered views). It gives
// embedders observability — host tests asserting on conversation structure,
// or persistence of the chat — without growing the mutation surface.
func (t *Transcript) Messages() []*types.Message {
	return t.msgs
}
