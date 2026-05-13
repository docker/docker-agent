// Package harness defines the cross-harness orchestration layer for docker-agent.
// It provides a common interface for dispatching sub-sessions to external agent
// runtimes (Claude Code, Codex, OpenCode, Copilot CLI, OpenClaw) and normalizing
// their event streams into a canonical 14-event vocabulary (AG-UI naming).
//
// # Protocol classes
//
// Self-contained stream harnesses (claude-code, codex, opencode) spawn a subprocess,
// read NDJSON/JSONL from stdout, and execute all tools internally. The adapter is
// read-only: parse lines, translate, emit canonical events.
//
// ACP harnesses (copilot, openclaw) speak JSON-RPC 2.0 over stdio. They delegate
// some tool execution (fs/*, terminal/*) back to the host. Adapters implement
// ACPAdapter and receive ACPCallbacks from the runtime.
//
// # Canonical event vocabulary
//
// Events use AG-UI naming. The runtime translates canonical events to docker-agent
// internal runtime.Event types at the boundary (pkg/runtime/harness_delegation.go).
// Adapters never import pkg/runtime.
package harness

import (
	"context"
	"encoding/json"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
)

// ProtocolClass identifies the wire protocol a harness adapter uses.
type ProtocolClass string

const (
	// ProtocolStream is used by self-contained harnesses that write NDJSON/JSONL to stdout.
	ProtocolStream ProtocolClass = "stream"
	// ProtocolACP is used by harnesses that speak JSON-RPC 2.0 over stdio.
	ProtocolACP ProtocolClass = "acp"
)

// ErrorCode classifies terminal harness errors for the orchestrator.
type ErrorCode string

const (
	ErrCodeContextExhausted   ErrorCode = "context_exhausted"
	ErrCodeRateLimited        ErrorCode = "rate_limited"
	ErrCodeAuthFailed         ErrorCode = "auth_failed"
	ErrCodeHarnessCrashed     ErrorCode = "harness_crashed"
	ErrCodeHarnessTimeout     ErrorCode = "harness_timeout"
	ErrCodeUserCanceled       ErrorCode = "user_canceled"
	ErrCodeCapabilityMismatch ErrorCode = "capability_mismatch"
	ErrCodeUnknown            ErrorCode = "unknown"
)

// HostRequirements declares what the host must provide for this adapter to function.
type HostRequirements struct {
	// ToolExecutor must be non-nil in ACPCallbacks when true.
	ToolExecutor bool
	// Permission must be non-nil in ACPCallbacks when true.
	Permission bool
}

// AdapterFeatures declares optional capabilities this adapter supports.
type AdapterFeatures struct {
	// SystemPrompt: adapter accepts SubSessionRequest.SystemPrompt.
	SystemPrompt bool
	// Reasoning: adapter emits ReasoningStart/Delta/End events.
	Reasoning bool
	// TextDeltas: adapter emits TextDelta events (not just TextStart/End).
	TextDeltas bool
	// MultiTurn: adapter supports native session resume via ResumeToken.
	MultiTurn bool
	// StreamingArgs: adapter emits ToolCallArgsDelta events.
	StreamingArgs bool
}

// AdapterCapabilities describes what an adapter can do and what it requires from the host.
// Capabilities() must be a pure function: no side effects, no process spawn.
type AdapterCapabilities struct {
	Protocol     ProtocolClass
	Requires     HostRequirements
	Features     AdapterFeatures
	// BuiltInTools lists tools the harness executes internally (informational only).
	BuiltInTools []string
}

// UsageSummary carries token and cost information from a completed run.
type UsageSummary struct {
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int     // Claude-specific
	CacheReadTokens     int     // Claude-specific
	ReasoningTokens     int     // o1/Codex
	CostUSD             float64 // when available
	DurationMS          int64
}

// SubSessionRequest is the input to HarnessAdapter.Run and ACPAdapter.RunACP.
type SubSessionRequest struct {
	RunID, ParentID string

	// SystemPrompt is the agent's instruction. Some adapters (OpenCode CLI) do
	// not support per-call system prompts; they prepend it to Task and warn.
	SystemPrompt string

	// Task is the user message / task description for this sub-session.
	Task string

	// ResumeToken is an adapter-opaque token from a prior RunEnd.HarnessRunID.
	// Non-empty means resume mode: the adapter uses native session resume and
	// ignores SimulatedHistory.
	ResumeToken string

	// SimulatedHistory is prior conversation turns to prepend to the system prompt.
	// Only used when ResumeToken == "" (first turn or harness lacks native resume).
	SimulatedHistory []chat.Message

	WorkingDir string
	Env        map[string]string

	// Config is the adapter-specific config from HarnessConfig.Config, marshaled
	// to JSON for the adapter to unmarshal into its own typed struct.
	Config json.RawMessage

	Timeout time.Duration
	Events  EventSink
}

// ACPCallbacks provides host-side services required by ACP adapters.
// The runtime validates that non-nil values are present when the adapter's
// Capabilities().Requires fields are true.
type ACPCallbacks struct {
	ToolExecutor ToolExecutor
	Permission   PermissionRequester
}

// HarnessAdapter is the base interface all harness adapters implement.
//
// Run MUST NOT return an error. All terminal states (success, error, crash)
// flow through req.Events as RunEnd or RunError events. The runtime wraps
// Run in a goroutine with recover() to catch panics and convert them to
// RunError{Code: ErrCodeHarnessCrashed}.
//
// Run MUST emit exactly one RunStart and exactly one RunEnd or RunError.
// Run MUST emit a Heartbeat at least every 30 seconds during active processing.
type HarnessAdapter interface {
	// Name returns the harness type identifier (e.g. "claude-code").
	Name() string
	// Capabilities returns the static capability declaration. Pure function.
	Capabilities() AdapterCapabilities
	// Run executes one sub-session. See interface doc for contract.
	Run(ctx context.Context, req SubSessionRequest)
}

// ACPAdapter extends HarnessAdapter for adapters that use the ACP protocol.
// The runtime detects this interface and calls RunACP instead of Run,
// providing the ACPCallbacks required for bidirectional tool execution.
type ACPAdapter interface {
	HarnessAdapter
	// RunACP executes one ACP sub-session with host-provided tool execution
	// and permission callbacks.
	RunACP(ctx context.Context, req SubSessionRequest, acp ACPCallbacks)
}
