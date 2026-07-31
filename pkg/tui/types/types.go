package types

import (
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/tools"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
)

// MessageType represents different types of messages
type MessageType int

const (
	MessageTypeUser MessageType = iota
	MessageTypeAssistant
	MessageTypeAssistantReasoningBlock // Collapsed reasoning + tool calls block
	MessageTypeSpinner
	MessageTypeError
	MessageTypeShellOutput
	MessageTypeCancelled
	MessageTypeToolCall
	MessageTypeToolResult
	MessageTypeWelcome
	MessageTypeLoading
	// MessageTypeAgentReturn is the UI-only delegation-return transition shown
	// when a sub-agent hands control back to its parent. It is live feedback
	// for the AgentSwitching runtime event, which is not persisted to the
	// session, so it never reappears when a session is reloaded.
	MessageTypeAgentReturn
)

const (
	UserMessageEditLabel = "✎ edit"
	MessageCopyLabel     = "⎘ copy"
	ErrorRetryLabel      = "↻ retry"
	// AgentReturnLabel is the connector between the child and parent badges
	// of a delegation-return transition (see AgentReturn).
	AgentReturnLabel = "returned control to"
	// MessageActionSeparator joins adjacent action labels (e.g. edit + copy)
	// on a message's hover-action row.
	MessageActionSeparator = "  "
	// CopiedFeedbackLabel transiently replaces a clicked copy label. It must
	// keep the exact display width of the copy labels so the swap never
	// shifts the layout of the line it happens on.
	CopiedFeedbackLabel = "copied"
)

const (
	maxLiveToolOutputSize        = 100_000
	liveToolOutputTruncatedLabel = "[earlier live output truncated]\n"
)

// ToolStatus represents the status of a tool call
type ToolStatus int

const (
	ToolStatusPending ToolStatus = iota
	ToolStatusConfirmation
	ToolStatusRunning
	ToolStatusCompleted
	ToolStatusError
)

// AssistantMedia is one generated-media item attached to an assistant
// message (an artifact-backed image produced by the model itself, resolved
// by the chat page). Image is non-nil only when the artifact bytes were
// resolved and decoded for terminal rendering; Fallback always carries the
// safe textual description shown when inline rendering is unavailable
// (graphics disabled, undecodable bytes, or unresolvable artifact).
type AssistantMedia struct {
	// ID links an item awaiting asynchronous resolution to the result that
	// replaces it (see messages.Model.UpdateAssistantMedia). Zero means
	// static: the item is final and never replaced.
	ID       uint64
	Image    *tuiimage.Inline
	Fallback string
}

// Message represents a single message in the chat
type Message struct {
	Type           MessageType
	Content        string
	Sender         string                // Agent name for assistant messages
	ToolCall       tools.ToolCall        // Associated tool call for tool messages
	ToolDefinition tools.Tool            // Definition of the tool being called
	ToolStatus     ToolStatus            // Status for tool calls
	ToolResult     *tools.ToolCallResult // Result of tool call (when completed)
	Images         []tuiimage.Inline     // Prepared terminal images from the result
	// AssistantMedia holds generated media rendered as part of an assistant
	// turn, after the message's text content.
	AssistantMedia []AssistantMedia
	// StartedAt records when a tool call entered ToolStatusRunning.
	// Used to display elapsed time for long-running tool calls.
	StartedAt *time.Time
	// SessionPosition is the index of this message in session.Messages (when known).
	// Used for operations like branching on edits.
	SessionPosition *int
}

func Agent(typ MessageType, agentName, content string) *Message {
	return &Message{
		Type:    typ,
		Sender:  agentName,
		Content: strings.ReplaceAll(content, "\t", "    "),
	}
}

func ShellOutput(content string) *Message {
	return &Message{
		Type:    MessageTypeShellOutput,
		Content: strings.ReplaceAll(content, "\t", "    "),
	}
}

func Spinner() *Message {
	return &Message{
		Type: MessageTypeSpinner,
	}
}

// SpinnerLabeled is a pending-response spinner that names the agent we're waiting
// on. Sender drives the accent color; Content holds the label (e.g. "root → x").
// Empty Content renders the default spinner.
func SpinnerLabeled(sender, label string) *Message {
	return &Message{Type: MessageTypeSpinner, Sender: sender, Content: label}
}

func Error(content string) *Message {
	return &Message{
		Type:    MessageTypeError,
		Content: strings.ReplaceAll(content, "\t", "    "),
	}
}

func User(content string) *Message {
	return &Message{
		Type:    MessageTypeUser,
		Content: strings.ReplaceAll(content, "\t", "    "),
	}
}

func Cancelled() *Message {
	return &Message{
		Type: MessageTypeCancelled,
	}
}

// AgentReturn is the delegation-return transition: fromAgent (the child)
// returned control to toAgent (the parent). Sender carries the child so the
// accent/badge machinery colors it like any other agent attribution; Content
// carries the parent name rather than copyable text — the message type is
// UI-only and is neither selectable nor copyable.
func AgentReturn(fromAgent, toAgent string) *Message {
	return &Message{
		Type:    MessageTypeAgentReturn,
		Sender:  fromAgent,
		Content: toAgent,
	}
}

func Welcome(content string) *Message {
	return &Message{
		Type:    MessageTypeWelcome,
		Content: strings.ReplaceAll(content, "\t", "    "),
	}
}

func ToolCallMessage(agentName string, toolCall tools.ToolCall, toolDef tools.Tool, status ToolStatus) *Message {
	msg := &Message{
		Type:           MessageTypeToolCall,
		Sender:         agentName,
		ToolCall:       toolCall,
		ToolDefinition: toolDef,
		ToolStatus:     status,
	}
	if status == ToolStatusRunning {
		now := time.Now()
		msg.StartedAt = &now
	}
	return msg
}

func (m *Message) AppendToolOutput(output string) {
	if output == "" {
		return
	}
	combined := m.Content + strings.ReplaceAll(output, "\t", "    ")
	if len(combined) <= maxLiveToolOutputSize {
		m.Content = combined
		return
	}

	tailSize := maxLiveToolOutputSize - len(liveToolOutputTruncatedLabel)
	if tailSize <= 0 {
		m.Content = combined[len(combined)-maxLiveToolOutputSize:]
		return
	}
	m.Content = liveToolOutputTruncatedLabel + combined[len(combined)-tailSize:]
}

func Loading(description string) *Message {
	return &Message{
		Type:    MessageTypeLoading,
		Content: strings.ReplaceAll(description, "\t", "    "),
	}
}
