package harness

import "time"

// Event is the sealed interface for all canonical harness events.
// Use a type switch to handle specific event types. The isHarnessEvent()
// method is unexported to prevent external implementations.
type Event interface {
	isHarnessEvent()
	// EventTime returns the wall-clock time the event was produced.
	EventTime() time.Time
}

// RunStart signals the beginning of a harness sub-session.
type RunStart struct {
	// RunID is the docker-agent sub-session ID.
	RunID string
	// HarnessRunID is the harness-native session ID (e.g. Claude Code session UUID).
	HarnessRunID string
	// ThreadID is the harness-native thread/conversation ID (e.g. Codex thread_id).
	ThreadID string
	At       time.Time
}

func (RunStart) isHarnessEvent()        {}
func (e RunStart) EventTime() time.Time { return e.At }

// TextStart opens a new assistant text message region.
type TextStart struct {
	MessageID string
	Role      string // typically "assistant"
	At        time.Time
}

func (TextStart) isHarnessEvent()        {}
func (e TextStart) EventTime() time.Time { return e.At }

// TextDelta delivers a streaming text chunk. Only emitted when
// AdapterFeatures.TextDeltas is true; otherwise the full text arrives in TextEnd.
type TextDelta struct {
	MessageID string
	Delta     string
	At        time.Time
}

func (TextDelta) isHarnessEvent()        {}
func (e TextDelta) EventTime() time.Time { return e.At }

// TextEnd closes a text message region. FinalText is the complete text for
// non-streaming harnesses (Codex, OpenCode); accumulate Deltas for streaming ones.
type TextEnd struct {
	MessageID string
	At        time.Time
}

func (TextEnd) isHarnessEvent()        {}
func (e TextEnd) EventTime() time.Time { return e.At }

// ReasoningStart opens a reasoning/thinking block.
type ReasoningStart struct {
	MessageID string
	At        time.Time
}

func (ReasoningStart) isHarnessEvent()        {}
func (e ReasoningStart) EventTime() time.Time { return e.At }

// ReasoningDelta delivers a streaming reasoning chunk.
type ReasoningDelta struct {
	MessageID string
	Delta     string
	At        time.Time
}

func (ReasoningDelta) isHarnessEvent()        {}
func (e ReasoningDelta) EventTime() time.Time { return e.At }

// ReasoningEnd closes a reasoning block.
type ReasoningEnd struct {
	MessageID string
	At        time.Time
}

func (ReasoningEnd) isHarnessEvent()        {}
func (e ReasoningEnd) EventTime() time.Time { return e.At }

// ToolCallStart opens a tool call. Args may follow as ToolCallArgsDelta events
// when AdapterFeatures.StreamingArgs is true. For non-streaming harnesses,
// Args contains the complete tool arguments as a JSON string.
type ToolCallStart struct {
	ToolCallID string
	ToolName   string
	Args       string // JSON-encoded tool arguments; may be empty for streaming harnesses
	At         time.Time
}

func (ToolCallStart) isHarnessEvent()        {}
func (e ToolCallStart) EventTime() time.Time { return e.At }

// ToolCallArgsDelta delivers a streaming tool argument chunk.
// Only emitted when AdapterFeatures.StreamingArgs is true.
type ToolCallArgsDelta struct {
	ToolCallID string
	Delta      string
	At         time.Time
}

func (ToolCallArgsDelta) isHarnessEvent()        {}
func (e ToolCallArgsDelta) EventTime() time.Time { return e.At }

// ToolCallEnd closes a tool call. A ToolCallResult follows.
type ToolCallEnd struct {
	ToolCallID string
	At         time.Time
}

func (ToolCallEnd) isHarnessEvent()        {}
func (e ToolCallEnd) EventTime() time.Time { return e.At }

// ToolCallResult delivers the result of a completed tool call.
// For atomic harnesses (Codex, OpenCode), ToolCallStart and ToolCallResult
// are emitted back-to-back with no ToolCallEnd in between.
type ToolCallResult struct {
	ToolCallID string
	ToolName   string
	Result     string
	IsError    bool
	At         time.Time
}

func (ToolCallResult) isHarnessEvent()        {}
func (e ToolCallResult) EventTime() time.Time { return e.At }

// PermissionPending signals that the harness is waiting for a permission decision.
// The runtime emits a ToolCallConfirmationEvent to the TUI and calls
// PermissionRequester.Request synchronously.
type PermissionPending struct {
	RequestID   string
	ToolCallID  string
	Description string
	Options     []string
	At          time.Time
}

func (PermissionPending) isHarnessEvent()        {}
func (e PermissionPending) EventTime() time.Time { return e.At }

// PermissionResolved signals the outcome of a permission decision.
type PermissionResolved struct {
	RequestID string
	Allowed   bool
	// Source records who made the decision: "user", "policy", "remembered", "timeout".
	Source string
	At     time.Time
}

func (PermissionResolved) isHarnessEvent()        {}
func (e PermissionResolved) EventTime() time.Time { return e.At }

// Heartbeat signals the adapter is alive during a long-running operation.
// Adapters MUST emit at least one Heartbeat every 30 seconds during active runs.
type Heartbeat struct {
	At time.Time
}

func (Heartbeat) isHarnessEvent()        {}
func (e Heartbeat) EventTime() time.Time { return e.At }

// RunEnd signals successful completion of a harness sub-session.
// HarnessRunID should be stored as the resume token for multi-turn sessions.
type RunEnd struct {
	RunID      string
	// HarnessRunID is the adapter-opaque token for session resumption.
	// Store via session.SetHarnessToken(agentName, HarnessRunID).
	HarnessRunID string
	Usage        *UsageSummary
	StopReason   string
	At           time.Time
}

func (RunEnd) isHarnessEvent()        {}
func (e RunEnd) EventTime() time.Time { return e.At }

// RunError signals terminal failure of a harness sub-session.
type RunError struct {
	RunID   string
	Code    ErrorCode
	Message string
	At      time.Time
}

func (RunError) isHarnessEvent()        {}
func (e RunError) EventTime() time.Time { return e.At }
