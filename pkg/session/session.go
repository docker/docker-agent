package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// defaultNewID returns a fresh random session ID.
func defaultNewID() string { return uuid.New().String() }

const (
	// toolContentPlaceholder is the text used to replace truncated tool content
	toolContentPlaceholder = "[content truncated]"

	// toolResultTruncationMarker replaces the removed middle of a tool result
	// that exceeds MaxToolResultTokens.
	toolResultTruncationMarker = "\n[...tool result truncated: middle omitted...]\n"
)

// SafetyPolicy is the per-session safety mode. The runtime routes
// tool calls to allow/ask/deny through the (mode × safety-label)
// table in pkg/runtime/toolexec; custom permission rules always win
// over the mode.
//
// Empty means "never explicitly chosen": tool calls behave like the
// pre-modes default (read-only-annotated tools auto-approve, everything
// else asks), except that ToolsApproved=true upgrades it to Autonomous.
type SafetyPolicy string

const (
	// SafetyPolicyStrict prompts on every tool call, including
	// read-only ones. Only custom allow rules silence a prompt.
	SafetyPolicyStrict SafetyPolicy = "strict"
	// SafetyPolicyBalanced auto-approves classifier-safe calls
	// (safe-listed shell commands, read-only-annotated tools); asks
	// on destructive and unknown.
	SafetyPolicyBalanced SafetyPolicy = "balanced"
	// SafetyPolicyRestricted is the fail-closed mode for unattended /
	// headless runs: classifier-safe calls auto-approve, destructive
	// and unknown calls are denied without ever prompting. Custom
	// rules still win (an explicit allow can approve a destructive
	// call, a deny always blocks, a session ask still prompts).
	SafetyPolicyRestricted SafetyPolicy = "restricted"
	// SafetyPolicyAutonomous auto-approves every call (legacy yolo).
	// Only custom deny/ask rules and preempt hooks still gate.
	SafetyPolicyAutonomous SafetyPolicy = "autonomous"
)

// Legacy safety-policy values accepted on input (API requests,
// persisted sessions) and normalized by [SafetyPolicy.Normalize].
// Never emitted by new code.
const (
	legacyPolicyUnsafe   SafetyPolicy = "unsafe"
	legacyPolicySafer    SafetyPolicy = "safer"
	legacyPolicySafeAuto SafetyPolicy = "safe-auto"
)

// Normalize maps legacy policy values onto the current mode
// vocabulary: unsafe → autonomous, safer / safe-auto → balanced
// (the cautious mapping: old "safer" also waved unknown calls
// through, balanced asks about them). Current values and empty pass
// through unchanged; unrecognised values collapse to strict so an
// unknown input can never widen approval.
func (p SafetyPolicy) Normalize() SafetyPolicy {
	switch p {
	case "", SafetyPolicyStrict, SafetyPolicyBalanced, SafetyPolicyRestricted, SafetyPolicyAutonomous:
		return p
	case legacyPolicyUnsafe:
		return SafetyPolicyAutonomous
	case legacyPolicySafer, legacyPolicySafeAuto:
		return SafetyPolicyBalanced
	default:
		return SafetyPolicyStrict
	}
}

// MinSafetyPolicy returns the more restrictive of two concrete safety modes.
// Under NonInteractive, strict and balanced deny calls that need confirmation,
// restricted permits only classifier-safe calls, and autonomous never asks.
// Empty is an unset legacy mode rather than an ordered policy, so an empty input
// yields empty; callers applying a policy ceiling must handle it explicitly.
func MinSafetyPolicy(a, b SafetyPolicy) SafetyPolicy {
	a = a.Normalize()
	b = b.Normalize()
	if a == "" || b == "" {
		return ""
	}

	if safetyPolicyRank(a) <= safetyPolicyRank(b) {
		return a
	}
	return b
}

func safetyPolicyRank(policy SafetyPolicy) int {
	switch policy {
	case SafetyPolicyStrict:
		return 0
	case SafetyPolicyBalanced:
		return 1
	case SafetyPolicyRestricted:
		return 2
	case SafetyPolicyAutonomous:
		return 3
	default:
		return 0
	}
}

// IsValid accepts current values, the legacy aliases, and empty.
func (p SafetyPolicy) IsValid() bool {
	switch p {
	case "", SafetyPolicyStrict, SafetyPolicyBalanced, SafetyPolicyRestricted, SafetyPolicyAutonomous,
		legacyPolicyUnsafe, legacyPolicySafer, legacyPolicySafeAuto:
		return true
	}
	return false
}

// Item represents a message, a sub-session, a summary, or a recorded error.
type Item struct {
	// Message holds a regular conversation message
	Message *Message `json:"message,omitempty"`

	// SubSession holds a complete sub-session from task transfers
	SubSession *Session `json:"sub_session,omitempty"`

	// Error holds a recorded agent failure. Persisting failures lets the
	// error survive a session reload and travel with a shared JSON export
	// for diagnostics.
	Error *Error `json:"error,omitempty"`

	// Termination holds a structured, non-error run stop marker (e.g. a
	// budget ceiling) recorded by the evaluation pipeline. Storing it as an
	// item keeps the stop's chronological position across reloads and
	// JSON exports. Absent for runs that ended normally or with an error.
	Termination *Termination `json:"termination,omitempty"`

	// Summary is a summary of the session up until this point
	Summary string `json:"summary,omitempty"`

	// FirstKeptEntry is the index (into the session's Messages slice) of the
	// first message that was kept verbatim during compaction. Messages from
	// this index onward (up to the summary item itself) are appended after
	// the summary when reconstructing the conversation. A value of -1 (or 0
	// with no summary) means no messages were kept.
	FirstKeptEntry int `json:"first_kept_entry,omitempty"`

	// Cost tracks the cost of operations associated with this item that
	// don't produce a regular message (e.g., compaction/summarization).
	Cost float64 `json:"cost,omitempty"`

	// Model names the model behind Cost for items that don't carry a
	// regular message (e.g., compaction summaries), so cost-per-model
	// breakdowns can attribute the spend.
	Model string `json:"model,omitempty"`

	// Usage records the token usage behind Cost for items that don't
	// carry a regular message (e.g., compaction summaries).
	Usage *chat.Usage `json:"usage,omitempty"`

	// liveAttached marks a sub-session item appended by AddLiveSubSession
	// in this process, i.e. a sub-session that ran live and reported its
	// own cost through its own TokenUsageEvents. Deliberately unexported
	// and not persisted: after a reload the sub-session no longer emits
	// events, so it must count as embedded (see EmbeddedSubSessionCost).
	liveAttached bool
}

// IsMessage returns true if this item contains a message
func (si *Item) IsMessage() bool {
	return si.Message != nil
}

// IsSubSession returns true if this item contains a sub-session
func (si *Item) IsSubSession() bool {
	return si.SubSession != nil
}

// IsError returns true if this item contains a recorded error
func (si *Item) IsError() bool {
	return si.Error != nil
}

// IsTermination returns true if this item contains a termination marker
func (si *Item) IsTermination() bool {
	return si.Termination != nil
}

// Error records an agent failure that occurred during a run. It is stored as
// a session item so the error is visible when the session is reopened and is
// included in a shared JSON session export for diagnostics.
type Error struct {
	// Message is the human-readable error message.
	Message string `json:"message"`
	// Code classifies the error (see runtime.ErrorCode* constants). Empty
	// when the error was emitted without a code.
	Code string `json:"code,omitempty"`
	// AgentName is the agent that was running when the error occurred.
	AgentName string `json:"agent_name,omitempty"`
	// CreatedAt is the RFC3339 timestamp of the failure.
	CreatedAt string `json:"created_at,omitempty"`
}

// TerminationReasonBudgetExceeded is the only recognized Termination
// reason: the runtime's native budget_exceeded event.
const TerminationReasonBudgetExceeded = "budget_exceeded"

// Termination records a structured, non-error run stop observed during an
// evaluation, currently only the runtime's budget_exceeded event. It is an
// allow-listed copy of that event: all fields are plain strings, only
// Reason is required, and producers omit optional fields whose source
// value is missing or unusable. It deliberately never carries session,
// agent, prompt, or tool data.
type Termination struct {
	// Reason is the stop reason; the only recognized value is
	// [TerminationReasonBudgetExceeded].
	Reason string `json:"reason"`
	// Budget names the budget that tripped: "run" for the top-level
	// `budget:`, otherwise the key under `budgets:`.
	Budget string `json:"budget,omitempty"`
	// Limit is the limit kind that tripped ("max_cost", "max_tokens",
	// "max_time").
	Limit string `json:"limit,omitempty"`
	// Used and Max are the human-readable amounts reported by the runtime.
	Used string `json:"used,omitempty"`
	Max  string `json:"max,omitempty"`
	// ConfigPath is the YAML path of the limit that tripped, e.g.
	// "budgets.tight.max_cost".
	ConfigPath string `json:"config_path,omitempty"`
	// Message is the human-readable stop message reported by the runtime.
	Message string `json:"message,omitempty"`
}

// Session represents the agent's state including conversation history and variables
type Session struct {
	// mu protects Messages and metadata that is written cross-goroutine
	// (Title, Attributes, InputTokens, OutputTokens, Cost, ...) from concurrent
	// read/write access. Shared-session readers must use the locked accessors.
	mu sync.RWMutex `json:"-"`

	// now and newID are per-session sources of time and identity. They are
	// indirected (rather than calling time.Now/uuid.New directly) so that
	// tests can inject a deterministic clock and ID generator via WithClock
	// and WithIDGen without mutating any process-global state — which keeps
	// such tests safe to run with t.Parallel(). Sessions created outside New
	// (e.g. via JSON deserialization or struct literals) leave these nil; use
	// the now() and newID() accessors which fall back to real implementations.
	clock func() time.Time `json:"-"`
	idgen func() string    `json:"-"`

	// ID is the unique identifier for the session
	ID string `json:"id"`

	// Origin identifies the protocol surface that created this session.
	Origin string `json:"origin,omitempty"`

	// InputID is an optional caller-supplied correlation ID read from the eval
	// input file's "input_id" field. It is carried through to the output as-is
	// and never used internally. The session's own "id" is always a fresh UUID.
	InputID string `json:"input_id,omitempty"`

	// Title is the title of the session, set by the runtime. Protected by
	// mu: shared-session callers must go through SetTitle/TitleSnapshot.
	Title string `json:"title"`

	// Evals contains evaluation criteria for this session (used by eval framework)
	Evals *EvalCriteria `json:"evals,omitempty"`

	// EvalResult contains the evaluation scoring outcome (populated after eval run).
	EvalResult *EvalResult `json:"eval_result,omitempty"`

	// Messages holds the conversation history (messages and sub-sessions)
	Messages []Item `json:"messages"`

	// CreatedAt is the time the session was created
	CreatedAt time.Time `json:"created_at"`

	// ToolsApproved is the legacy --yolo signal. New code should
	// prefer SafetyPolicy; option setters keep the two in sync.
	ToolsApproved bool `json:"tools_approved"`

	// SafetyPolicy is the per-session safety preference. See the
	// [SafetyPolicy] type doc for the modes and empty-value semantics.
	SafetyPolicy SafetyPolicy `json:"safety_policy,omitempty"`

	// PriorSafetyPolicy remembers the mode that was active before a yolo
	// toggle escalated to Autonomous, so toggling off restores it instead
	// of discarding an explicit Balanced/Strict choice. Managed by
	// [Session.ToggleYolo]; empty outside a toggle escalation.
	PriorSafetyPolicy SafetyPolicy `json:"prior_safety_policy,omitempty"`

	// NonInteractive indicates the session is running in a non-interactive context
	// (e.g. MCP server, A2A adapter, evaluation framework) where there is no user
	// to provide input. This is distinct from ToolsApproved which can also be set
	// in interactive TUI sessions when a user approves all tools.
	NonInteractive bool `json:"non_interactive,omitempty"`

	// HideToolResults is a flag to indicate if tool results should be hidden
	HideToolResults bool `json:"hide_tool_results"`

	// WorkingDir is the base directory used for filesystem-aware tools
	WorkingDir string `json:"working_dir,omitempty"`

	// SendUserMessage is a flag to indicate if the user message should be sent
	SendUserMessage bool

	// MaxIterations is the maximum number of agentic loop iterations to prevent infinite loops
	// If 0, there is no limit
	MaxIterations int `json:"max_iterations"`

	// MaxConsecutiveToolCalls is the maximum number of consecutive identical tool call
	// batches before the agent is terminated. Prevents degenerate loops where the model
	// repeatedly issues the same call without making progress. Default: 5.
	MaxConsecutiveToolCalls int `json:"max_consecutive_tool_calls,omitempty"`

	// MaxOldToolCallTokens is the maximum number of tokens to keep from old tool call
	// arguments and results. Older tool calls beyond this budget will have their
	// content replaced with a placeholder. Tokens are approximated by
	// approximateTokens (len/4). Truncation is enabled only when this is positive;
	// 0 (unset) and -1 both disable truncation (unlimited tool content).
	MaxOldToolCallTokens int `json:"max_old_tool_call_tokens,omitempty"`

	// MaxToolResultTokens is the maximum number of tokens to keep from each
	// textual tool result at ingestion. Oversized results are middle-out
	// truncated by AddMessage: the head and tail are kept and the removed
	// middle is replaced with toolResultTruncationMarker. The budget covers
	// every textual payload of the result — Content and inline-text document
	// parts share it rather than each claiming a full cap. Tokens are
	// approximated by approximateTokens (len/4). The cap is enabled only when
	// this is positive; 0 (unset) and -1 both disable it (unbounded results).
	// GetMessages reapplies the same cap to the copies it returns as a
	// backstop for results that entered the history without passing through
	// AddMessage (e.g. persisted via the API/SQLite path and reloaded).
	MaxToolResultTokens int `json:"max_tool_result_tokens,omitempty"`

	// Starred indicates if this session has been starred by the user
	Starred bool `json:"starred"`

	// InputTokens, OutputTokens, and Cost are cumulative usage counters
	// protected by mu: shared-session callers must go through
	// SetTokensAndCost/SetUsage and TokensAndCost/Usage.
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	Cost         float64 `json:"cost"`

	// Permissions holds session-level permission overrides.
	// When set, these are evaluated before team-level permissions.
	Permissions *PermissionsConfig `json:"permissions,omitempty"`

	// Attributes stores generic, namespaced metadata supplied by embedders.
	// Shared-session callers must use AttributesSnapshot, SetAttribute, and
	// DeleteAttribute rather than mutating this map directly.
	Attributes map[string]string `json:"attributes,omitempty"`

	// AgentModelOverrides stores per-agent model overrides for this session.
	// Key is the agent name, value is the model reference (e.g., "openai/gpt-4o" or a named model from config).
	// When a session is loaded, these overrides are reapplied to the runtime.
	AgentModelOverrides map[string]string `json:"agent_model_overrides,omitempty"`

	// CustomModelsUsed tracks custom models (provider/model format) used during this session.
	// These are shown in the model picker for easy re-selection.
	CustomModelsUsed []string `json:"custom_models_used,omitempty"`

	// AttachedFiles records absolute paths of files the user attached to this
	// session via the editor's @-mentions, the in-message /attach directive, or
	// the CLI --attach flag. Sub-sessions created via task transfer inherit
	// this list so that delegated agents can reference the same files without
	// having to scan the workspace or guess from a bare filename. Paths are
	// deduplicated and order-preserved.
	AttachedFiles []string `json:"attached_files,omitempty"`

	// ExcludedTools lists tool names that should be filtered out of the agent's
	// tool list for this session. This is used by skill sub-sessions to prevent
	// recursive run_skill calls.
	ExcludedTools []string `json:"-"`

	// AllowedTools, when non-empty, restricts this session's agent tools to
	// those whose names match an entry (filepath.Match-style glob, falling
	// back to an exact match). Used by fork-mode skill sub-sessions that
	// declare an allowed-tools list. An empty list imposes no restriction.
	// ExtraToolSets are always kept regardless of this filter.
	AllowedTools []string `json:"-"`

	// ExtraToolSets holds additional toolsets injected into this session on
	// top of the agent's own toolsets. Used by fork-mode skill sub-sessions
	// that declare assistive toolsets. Their tools bypass the AllowedTools
	// filter (the skill explicitly asked for them).
	ExtraToolSets []tools.ToolSet `json:"-"`

	// DisableStructuredOutput, when true, exempts this session from tool-mode
	// structured-output enforcement: the internal output tool is not offered
	// and plain-text finals are accepted. Used by fork-mode skill
	// sub-sessions, whose answer is free-form text for the calling agent, not
	// the agent's schema-constrained final output. Not persisted: like the
	// other transient sub-session state, a restored or branched session
	// starts with enforcement re-enabled.
	DisableStructuredOutput bool `json:"-"`

	// AgentName, when set, tells RunStream which agent to use for this session
	// instead of reading from the shared runtime currentAgent field. This is
	// required for background agent tasks where multiple sessions may run
	// concurrently on different agents.
	AgentName string `json:"-"`

	// ParentID indicates this is a sub-session created by task transfer.
	// Sub-sessions are not persisted as standalone entries; they are embedded
	// within the parent session's Messages array.
	ParentID string `json:"-"`

	// DelegationLineage records the names of the agents that delegated,
	// transitively, to produce this sub-session (root caller first), not
	// including the agent running the session itself. The runtime uses it to
	// reject delegation cycles and excessive nesting across transfer_task and
	// run_background_agent. Not persisted: a restored session starts as a
	// fresh delegation root.
	DelegationLineage []string `json:"-"`

	// InstructionContext keeps the cache-stable snapshot and chronological
	// updates for dynamic system context.
	InstructionContext *InstructionContextState `json:"instruction_context,omitempty"`

	// MessageUsageHistory stores per-message usage data for remote mode.
	// In remote mode, messages are managed server-side, so we track usage separately.
	// This is not persisted (json:"-") as it's only needed for the current session display.
	MessageUsageHistory []MessageUsageRecord `json:"-"`
}

// InstructionSource is one independently changing piece of trusted context.
type InstructionSource struct {
	Key            string
	Group          string
	Label          string
	Content        string
	ChangedContent string
	RemovedContent string
	Removed        bool
	Available      bool
	CompleteGroup  bool
	SetMarker      bool
}

// InstructionValue is a content-addressed instruction value.
type InstructionValue struct {
	Hash           string `json:"hash"`
	Group          string `json:"group,omitempty"`
	Label          string `json:"label,omitempty"`
	Content        string `json:"content"`
	RemovedContent string `json:"removed_content,omitempty"`
}

// InstructionUpdate records an append-only context change at a session item boundary.
type InstructionUpdate struct {
	Position int    `json:"position"`
	Content  string `json:"content"`
}

// InstructionContextState separates the frozen epoch snapshot from current values.
type InstructionContextState struct {
	EpochStart int                         `json:"epoch_start"`
	Order      []string                    `json:"order,omitempty"`
	Initial    map[string]InstructionValue `json:"initial,omitempty"`
	Current    map[string]InstructionValue `json:"current,omitempty"`
	Updates    []InstructionUpdate         `json:"updates,omitempty"`
}

// MessageUsageRecord stores usage data for a single assistant message.
// Used in remote mode where messages aren't stored in the client-side session.
type MessageUsageRecord struct {
	AgentName string     `json:"agent_name"`
	Model     string     `json:"model"`
	Cost      float64    `json:"cost"`
	Usage     chat.Usage `json:"usage"`
}

// PermissionsConfig defines session-level tool permission overrides
// using pattern-based rules (Allow/Ask/Deny arrays).
type PermissionsConfig struct {
	// Allow lists tool name patterns that are auto-approved without user confirmation.
	Allow []string `json:"allow,omitempty"`
	// Ask lists tool name patterns that always require user confirmation,
	// even for tools that are normally auto-approved (e.g. read-only tools).
	Ask []string `json:"ask,omitempty"`
	// Deny lists tool name patterns that are always rejected.
	Deny []string `json:"deny,omitempty"`
}

// Clone returns a deep copy of the permissions configuration.
func (c *PermissionsConfig) Clone() *PermissionsConfig {
	if c == nil {
		return nil
	}
	return &PermissionsConfig{
		Allow: slices.Clone(c.Allow),
		Ask:   slices.Clone(c.Ask),
		Deny:  slices.Clone(c.Deny),
	}
}

// Message is a message from an agent
type Message struct {
	// ID is the database ID of the message (used for persistence tracking)
	ID        int64        `json:"-"`
	AgentName string       `json:"agent_name"`
	Message   chat.Message `json:"message"`
	// Implicit is an optional field to indicate if the message shouldn't be shown to the user. It's needed for special  situations
	// like when an agent transfers a task to another agent - new session is created with a default user message, but this shouldn't be shown to the user.
	// Such messages should be marked as true
	Implicit bool `json:"implicit,omitempty"`
}

// UnmarshalJSON accepts both the current "agent_name" key and the legacy
// "agentName" key emitted by exports prior to the rename. When "agent_name"
// is absent or empty, a non-empty legacy value wins; absent and empty are
// deliberately not distinguished since no producer emits both keys.
func (m *Message) UnmarshalJSON(data []byte) error {
	type message Message // alias to avoid infinite recursion
	aux := struct {
		*message

		LegacyAgentName string `json:"agentName"`
	}{message: (*message)(m)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if m.AgentName == "" {
		m.AgentName = aux.LegacyAgentName
	}
	return nil
}

func ImplicitUserMessage(content string) *Message {
	return ImplicitUserMessageAt(time.Now(), content)
}

// ImplicitUserMessageAt is like ImplicitUserMessage but stamps the message with
// an explicit creation time, letting callers (and tests) avoid the wall clock.
func ImplicitUserMessageAt(createdAt time.Time, content string) *Message {
	msg := UserMessageAt(createdAt, content)
	msg.Implicit = true
	return msg
}

func UserMessage(content string, multiContent ...chat.MessagePart) *Message {
	return UserMessageAt(time.Now(), content, multiContent...)
}

// UserMessageAt is like UserMessage but stamps the message with an explicit
// creation time, letting callers (and tests) avoid the wall clock.
func UserMessageAt(createdAt time.Time, content string, multiContent ...chat.MessagePart) *Message {
	return &Message{
		Message: chat.Message{
			Role:         chat.MessageRoleUser,
			Content:      content,
			MultiContent: multiContent,
			CreatedAt:    createdAt.Format(time.RFC3339),
		},
	}
}

func NewAgentMessage(agentName string, message *chat.Message) *Message {
	return &Message{
		AgentName: agentName,
		Message:   *message,
	}
}

func SystemMessage(content string) *Message {
	return SystemMessageAt(time.Now(), content)
}

// SystemMessageAt is like SystemMessage but stamps the message with an explicit
// creation time, letting callers (and tests) avoid the wall clock.
func SystemMessageAt(createdAt time.Time, content string) *Message {
	return &Message{
		Message: chat.Message{
			Role:      chat.MessageRoleSystem,
			Content:   content,
			CreatedAt: createdAt.Format(time.RFC3339),
		},
	}
}

// Helper functions for creating SessionItems

// NewMessageItem creates a SessionItem containing a message
func NewMessageItem(msg *Message) Item {
	return Item{Message: msg}
}

// NewSubSessionItem creates a SessionItem containing a sub-session
func NewSubSessionItem(subSession *Session) Item {
	return Item{SubSession: subSession}
}

// NewErrorItem creates a SessionItem containing a recorded error
func NewErrorItem(e *Error) Item {
	return Item{Error: e}
}

// NewTerminationItem creates a SessionItem containing a termination marker
func NewTerminationItem(t *Termination) Item {
	return Item{Termination: t}
}

// EvalResult contains the evaluation scoring outcome for a session.
type EvalResult struct {
	Passed       bool             `json:"passed"`
	Successes    []string         `json:"successes,omitempty"`
	Failures     []string         `json:"failures,omitempty"`
	Error        string           `json:"error,omitempty"`
	Cost         float64          `json:"cost"`
	OutputTokens int64            `json:"output_tokens"`
	Checks       EvalResultChecks `json:"checks"`
	// Termination is the structured, non-error stop recorded for the run
	// (e.g. budget exceeded), copied from the session items. It is
	// informational only and does not affect Passed, Failures, or Error.
	// Absent for runs that ended normally or with an error.
	Termination *Termination `json:"termination,omitempty"`
}

// EvalResultChecks groups the individual check results.
// Only checks that were evaluated will be present (omitted if nil).
type EvalResultChecks struct {
	Size      *SizeCheck      `json:"size,omitempty"`
	ToolCalls *ToolCallsCheck `json:"tool_calls,omitempty"`
	Relevance *RelevanceCheck `json:"relevance,omitempty"`
}

// SizeCheck contains the result of the response size check.
type SizeCheck struct {
	Passed   bool   `json:"passed"`
	Actual   string `json:"actual"`
	Expected string `json:"expected"`
}

// ToolCallsCheck contains the result of the tool calls F1 score check.
type ToolCallsCheck struct {
	Passed bool    `json:"passed"`
	Score  float64 `json:"score"`
}

// RelevanceCheck contains the result of the LLM judge relevance check.
type RelevanceCheck struct {
	Passed      bool                       `json:"passed"`
	PassedCount float64                    `json:"passed_count"`
	Total       float64                    `json:"total"`
	Results     []RelevanceCriterionResult `json:"results"`
}

// RelevanceCriterionResult contains the judge's verdict on a single relevance criterion.
type RelevanceCriterionResult struct {
	Criterion string `json:"criterion"`
	Passed    bool   `json:"passed"`
	Reason    string `json:"reason,omitempty"`
}

// EvalCriteria contains the evaluation criteria for a session.
type EvalCriteria struct {
	Relevance  []string `json:"relevance"`             // Statements that should be true about the response
	WorkingDir string   `json:"working_dir,omitempty"` // Subdirectory under evals/working_dirs/
	Size       string   `json:"size,omitempty"`        // Expected response size: S, M, L, XL
	Setup      string   `json:"setup,omitempty"`       // Optional sh script to run in the container before docker agent run --exec
	Image      string   `json:"image,omitempty"`       // Custom Docker image for this eval (overrides --base-image)
}

// UnmarshalJSON implements custom JSON unmarshaling for EvalCriteria that
// rejects unknown fields. This ensures eval JSON files don't contain typos
// or unsupported fields that would be silently ignored.
func (e *EvalCriteria) UnmarshalJSON(data []byte) error {
	type evalCriteria EvalCriteria // alias to avoid infinite recursion
	var v evalCriteria
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return err
	}
	*e = EvalCriteria(v)
	return nil
}

// cloneMessage returns a deep copy of a session Message.
// It copies the inner chat.Message's slice and pointer fields so that the
// returned value shares no mutable state with the original.
func cloneMessage(m *Message) *Message {
	cp := *m
	cp.Message = cloneChatMessage(m.Message)
	return &cp
}

// snapshotItems returns a copy of s.Messages safe to use without holding
// s.mu. Each Message value is deep-copied so concurrent UpdateMessage calls
// cannot mutate the snapshot; the remaining non-value fields (Summary,
// SubSession, Cost, FirstKeptEntry, Model) are shallow-copied since they are
// not mutated in place.
func (s *Session) snapshotItems() []Item {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]Item, len(s.Messages))
	for i, item := range s.Messages {
		items[i] = item
		if item.Message != nil {
			items[i].Message = cloneMessage(item.Message)
		}
		if item.Usage != nil {
			usage := *item.Usage
			items[i].Usage = &usage
		}
	}
	return items
}

// cloneChatMessage returns a deep copy of a chat.Message, duplicating
// all slice and pointer fields that would otherwise alias the original.
func cloneChatMessage(m chat.Message) chat.Message {
	if m.MultiContent != nil {
		orig := m.MultiContent
		m.MultiContent = make([]chat.MessagePart, len(orig))
		for i, part := range orig {
			if part.ImageURL != nil {
				imgCopy := *part.ImageURL
				part.ImageURL = &imgCopy
			}
			if part.File != nil {
				fileCopy := *part.File
				part.File = &fileCopy
			}
			if part.Document != nil {
				docCopy := *part.Document
				if part.Document.Source.InlineData != nil {
					docCopy.Source.InlineData = slices.Clone(part.Document.Source.InlineData)
				}
				part.Document = &docCopy
			}
			m.MultiContent[i] = part
		}
	}
	if m.FunctionCall != nil {
		fcCopy := *m.FunctionCall
		m.FunctionCall = &fcCopy
	}
	if m.ToolCalls != nil {
		m.ToolCalls = slices.Clone(m.ToolCalls)
	}
	if m.ToolDefinitions != nil {
		m.ToolDefinitions = cloneToolDefinitions(m.ToolDefinitions)
	}
	if m.Usage != nil {
		usageCopy := *m.Usage
		m.Usage = &usageCopy
	}
	if m.ThoughtSignature != nil {
		m.ThoughtSignature = slices.Clone(m.ThoughtSignature)
	}
	return m
}

func cloneToolDefinitions(src []tools.Tool) []tools.Tool {
	if src == nil {
		return nil
	}
	out := make([]tools.Tool, len(src))
	for i, tool := range src {
		out[i] = tool
		out[i].Parameters = cloneSchemaValue(tool.Parameters)
		out[i].OutputSchema = cloneSchemaValue(tool.OutputSchema)
		out[i].Annotations = cloneToolAnnotations(tool.Annotations)
	}
	return out
}

func cloneToolAnnotations(src tools.ToolAnnotations) tools.ToolAnnotations {
	cp := src
	if src.DestructiveHint != nil {
		hint := *src.DestructiveHint
		cp.DestructiveHint = &hint
	}
	if src.OpenWorldHint != nil {
		hint := *src.OpenWorldHint
		cp.OpenWorldHint = &hint
	}
	return cp
}

func cloneSchemaValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		cp := make(map[string]any, len(x))
		for k, v := range x {
			cp[k] = cloneSchemaValue(v)
		}
		return cp
	case []any:
		cp := make([]any, len(x))
		for i, v := range x {
			cp[i] = cloneSchemaValue(v)
		}
		return cp
	default:
		return v
	}
}

// Session helper methods

// AddMessage adds a message to the session and returns the index the new
// item occupies in s.Messages. Callers that need to stamp an event with the
// message's position (e.g. UserMessageEvent.SessionPosition) must use this
// return value rather than a separate len(sess.Messages)-1 read: the latter
// races with concurrent AddMessage/ApplyCompaction calls (e.g. from a live
// HTTP AddMessage while a stream is running) and can also observe a later,
// larger length than the one that matched this append.
func (s *Session) AddMessage(msg *Message) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg != nil {
		capToolResultContent(&msg.Message, s.MaxToolResultTokens)
		// Transcripts never carry cache checkpoint marks: CacheControl is
		// request-assembly state (see chat.Message.CacheControl); a mark
		// slipping in here (e.g. echoed back by an API client) would
		// resurface on every future prompt assembly.
		msg.Message.CacheControl = false
	}
	s.Messages = append(s.Messages, NewMessageItem(msg))
	return len(s.Messages) - 1
}

// MarshalJSON takes a consistent snapshot while encoding mutable session
// state. In particular, SetAttribute may otherwise mutate a map while the JSON
// encoder is iterating it.
func (s *Session) MarshalJSON() ([]byte, error) {
	if s == nil {
		return []byte("null"), nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	type sessionJSON Session
	return json.Marshal((*sessionJSON)(s))
}

// SetUsage records cumulative input/output token counts under s.mu.
// The runtime stream goroutine and the persistence observer race on
// these fields without it.
func (s *Session) SetUsage(input, output int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.InputTokens = input
	s.OutputTokens = output
}

// Usage returns a consistent snapshot of the cumulative input/output
// token counts.
func (s *Session) Usage() (input, output int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.InputTokens, s.OutputTokens
}

// TokensAndCost returns a consistent snapshot of the cumulative token
// counts together with the session-level cost, the read-side counterpart
// to SetTokensAndCost.
func (s *Session) TokensAndCost() (inputTokens, outputTokens int64, cost float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.InputTokens, s.OutputTokens, s.Cost
}

// SetTokensAndCost atomically records the cumulative token counts
// together with the session-level cost under s.mu. Granular store
// updates (e.g. InMemorySessionStore.UpdateSessionTokens) and event
// importers write these fields cross-goroutine with readers such as
// the persistence observer's UpdateSession snapshot, which reads them
// under the same lock.
func (s *Session) SetTokensAndCost(inputTokens, outputTokens int64, cost float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.InputTokens = inputTokens
	s.OutputTokens = outputTokens
	s.Cost = cost
}

// SetTitle updates the session title under s.mu. Title writers (title
// generation, HTTP title updates, granular store updates) run on
// different goroutines than readers like the UpdateSession snapshot,
// so direct field writes would race.
func (s *Session) SetTitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Title = title
}

// TitleSnapshot returns the session title under s.mu, the read-side
// counterpart to SetTitle. Named TitleSnapshot because the Title field
// and a method cannot share the name.
func (s *Session) TitleSnapshot() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Title
}

// ApplyCompaction atomically resets the session's cumulative token
// counts and appends a summary item under s.mu so concurrent readers
// (e.g. the persistence observer's UpdateSession snapshot) cannot
// observe the new tokens without the matching summary item.
func (s *Session) ApplyCompaction(inputTokens, outputTokens int64, item Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.InputTokens = inputTokens
	s.OutputTokens = outputTokens
	s.Messages = append(s.Messages, item)
}

// PrepareInstructionContext observes dynamic context before a model call. The
// initial snapshot remains frozen until compaction; later changes become
// append-only wrapped-user updates at the current history position.
func (s *Session) PrepareInstructionContext(sources []InstructionSource) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.InstructionContext == nil {
		for _, source := range sources {
			if !source.Available {
				return false
			}
		}
		state := &InstructionContextState{
			EpochStart: -1,
			Initial:    make(map[string]InstructionValue),
			Current:    make(map[string]InstructionValue),
		}
		for _, source := range sources {
			if !source.Available || source.SetMarker || source.Removed || strings.TrimSpace(source.Content) == "" {
				continue
			}
			value := instructionValue(source)
			state.Order = append(state.Order, source.Key)
			state.Initial[source.Key] = value
			state.Current[source.Key] = value
		}
		s.InstructionContext = state
		return true
	}

	state := s.InstructionContext
	latestSummary := -1
	for i := range slices.Backward(s.Messages) {
		if s.Messages[i].Summary != "" {
			latestSummary = i
			break
		}
	}
	changed := false
	if latestSummary > state.EpochStart {
		state.EpochStart = latestSummary
		state.Initial = cloneInstructionValues(state.Current)
		state.Updates = nil
		changed = true
	}

	completeGroups := make(map[string]bool)
	observedKeys := make(map[string]bool)
	for _, source := range sources {
		if source.Available && source.CompleteGroup && source.Group != "" {
			completeGroups[source.Group] = true
		}
		if source.Available && !source.SetMarker && source.Key != "" {
			observedKeys[source.Key] = true
		}
	}

	var rendered []string
	for _, source := range sources {
		if !source.Available || source.SetMarker || source.Key == "" {
			continue
		}
		previous, existed := state.Current[source.Key]
		if source.Removed || strings.TrimSpace(source.Content) == "" {
			if !existed {
				continue
			}
			delete(state.Current, source.Key)
			if source.RemovedContent != "" {
				rendered = append(rendered, source.RemovedContent)
			} else {
				rendered = append(rendered, "The context under \""+sourceLabel(source)+"\" no longer applies. Disregard it.")
			}
			continue
		}

		current := instructionValue(source)
		if existed && previous.Hash == current.Hash {
			continue
		}
		switch {
		case !existed:
			state.Order = appendMissing(state.Order, source.Key)
			rendered = append(rendered, "Additional context is now available under \""+sourceLabel(source)+"\":\n\n"+source.Content)
		case source.ChangedContent != "":
			rendered = append(rendered, source.ChangedContent)
		default:
			rendered = append(rendered, "The context under \""+sourceLabel(source)+"\" changed and supersedes the previous value:\n\n"+source.Content)
		}
		state.Current[source.Key] = current
	}
	for _, key := range state.Order {
		previous, exists := state.Current[key]
		if !exists || previous.Group == "" || !completeGroups[previous.Group] || observedKeys[key] {
			continue
		}
		delete(state.Current, key)
		if previous.RemovedContent != "" {
			rendered = append(rendered, previous.RemovedContent)
		} else {
			rendered = append(rendered, "The context under \""+previous.Label+"\" no longer applies. Disregard it.")
		}
	}
	if len(rendered) == 0 {
		return changed
	}

	state.Updates = append(state.Updates, InstructionUpdate{
		Position: len(s.Messages),
		Content:  "<system-update>\n" + strings.Join(rendered, "\n\n") + "\n</system-update>",
	})
	return true
}

func instructionValue(source InstructionSource) InstructionValue {
	encoded, _ := json.Marshal(source.Content)
	sum := sha256.Sum256(encoded)
	return InstructionValue{
		Hash:           hex.EncodeToString(sum[:]),
		Group:          source.Group,
		Label:          sourceLabel(source),
		Content:        source.Content,
		RemovedContent: source.RemovedContent,
	}
}

func sourceLabel(source InstructionSource) string {
	if source.Label != "" {
		return source.Label
	}
	return source.Key
}

func cloneInstructionValues(values map[string]InstructionValue) map[string]InstructionValue {
	return maps.Clone(values)
}

func appendMissing(values []string, value string) []string {
	if !slices.Contains(values, value) {
		return append(values, value)
	}
	return values
}

func cloneInstructionContext(state *InstructionContextState) *InstructionContextState {
	if state == nil {
		return nil
	}
	return &InstructionContextState{
		EpochStart: state.EpochStart,
		Order:      slices.Clone(state.Order),
		Initial:    cloneInstructionValues(state.Initial),
		Current:    cloneInstructionValues(state.Current),
		Updates:    slices.Clone(state.Updates),
	}
}

// AddSubSession adds a sub-session to the session as embedded history,
// e.g. when a store hydrates a parent with its children. Its cost counts
// as embedded (see EmbeddedSubSessionCost); use AddLiveSubSession for a
// sub-session that ran live in this process.
func (s *Session) AddSubSession(subSession *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = append(s.Messages, NewSubSessionItem(subSession))
}

// AddLiveSubSession adds a sub-session that ran live in this process: it
// has its own TokenUsageEvent entry, having reported its own cost through
// its own events, so EmbeddedSubSessionCost skips it to avoid double
// counting in per-session aggregations.
func (s *Session) AddLiveSubSession(subSession *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := NewSubSessionItem(subSession)
	item.liveAttached = true
	s.Messages = append(s.Messages, item)
}

// AddError appends a recorded error to the session so it survives reload and
// JSON export.
func (s *Session) AddError(e *Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = append(s.Messages, NewErrorItem(e))
}

// AddTermination appends a structured termination marker to the session so
// the stop's chronological position survives reload and JSON export.
func (s *Session) AddTermination(t *Termination) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = append(s.Messages, NewTerminationItem(t))
}

// Termination returns a copy of the first structured termination marker
// recorded in the session's items, or nil when the run was never stopped
// by one.
func (s *Session) Termination() *Termination {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.Messages {
		if s.Messages[i].Termination != nil {
			t := *s.Messages[i].Termination
			return &t
		}
	}
	return nil
}

// Duration calculates the duration of the session from message timestamps.
func (s *Session) Duration() time.Duration {
	messages := s.GetAllMessages()
	if len(messages) < 2 {
		return 0
	}

	first, err := time.Parse(time.RFC3339, messages[0].Message.CreatedAt)
	if err != nil {
		return 0
	}

	last, err := time.Parse(time.RFC3339, messages[len(messages)-1].Message.CreatedAt)
	if err != nil {
		return 0
	}

	return last.Sub(first)
}

// AllowedDirectories returns the directories that should be considered safe for tools
func (s *Session) AllowedDirectories() []string {
	if s.WorkingDir == "" {
		return nil
	}
	return []string{s.WorkingDir}
}

// GetAllMessages extracts all messages from the session, including from sub-sessions
func (s *Session) GetAllMessages() []Message {
	items := s.snapshotItems()

	var messages []Message
	for _, item := range items {
		if item.IsMessage() && item.Message.Message.Role != chat.MessageRoleSystem {
			messages = append(messages, *item.Message)
		} else if item.IsSubSession() {
			// Recursively get messages from sub-sessions
			subMessages := item.SubSession.GetAllMessages()
			messages = append(messages, subMessages...)
		}
	}
	return messages
}

// GetAllErrors extracts all recorded errors from the session, including from
// sub-sessions, in item order. Recorded errors live alongside messages as
// session items but are not part of the conversation, so exports that build
// on GetAllMessages need this to surface failures for diagnostics.
func (s *Session) GetAllErrors() []Error {
	items := s.snapshotItems()

	var errs []Error
	for _, item := range items {
		if item.IsError() {
			errs = append(errs, *item.Error)
		} else if item.IsSubSession() {
			errs = append(errs, item.SubSession.GetAllErrors()...)
		}
	}
	return errs
}

// OwnMessages extracts this session's direct messages, excluding system
// messages and WITHOUT recursing into sub-sessions. This is the set of
// messages that actually enters this session's prompt (GetMessages skips
// sub-session items), so token accounting that drives compaction must
// use it: counting sub-session content would attribute phantom tokens
// to the parent and compact a conversation that isn't actually large.
func (s *Session) OwnMessages() []Message {
	items := s.snapshotItems()

	var messages []Message
	for _, item := range items {
		if item.IsMessage() && item.Message.Message.Role != chat.MessageRoleSystem {
			messages = append(messages, *item.Message)
		}
	}
	return messages
}

func (s *Session) GetLastAssistantMessageContent() string {
	return s.getLastMessageContentByRole(chat.MessageRoleAssistant)
}

func (s *Session) GetLastUserMessageContent() string {
	return s.getLastMessageContentByRole(chat.MessageRoleUser)
}

// GetLastUserMessages returns up to n most recent user messages, ordered from oldest to newest.
// Returns nil if n <= 0.
func (s *Session) GetLastUserMessages(n int) []string {
	if n <= 0 {
		return nil
	}
	messages := s.GetAllMessages()
	var userMessages []string
	for i := range messages {
		if messages[i].Message.Role == chat.MessageRoleUser {
			content := strings.TrimSpace(messages[i].Message.Content)
			if content != "" {
				userMessages = append(userMessages, content)
			}
		}
	}
	if len(userMessages) <= n {
		return userMessages
	}
	return userMessages[len(userMessages)-n:]
}

func (s *Session) getLastMessageContentByRole(role chat.MessageRole) string {
	messages := s.GetAllMessages()
	for _, message := range slices.Backward(messages) {
		if message.Message.Role == role {
			return strings.TrimSpace(message.Message.Content)
		}
	}
	return ""
}

// AddMessageUsageRecord appends a usage record for remote mode where messages aren't stored locally.
// This enables the /cost dialog to show per-message breakdown even when using a remote runtime.
func (s *Session) AddMessageUsageRecord(agentName, model string, cost float64, usage *chat.Usage) {
	if usage == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.MessageUsageHistory = append(s.MessageUsageHistory, MessageUsageRecord{
		AgentName: agentName,
		Model:     model,
		Cost:      cost,
		Usage:     *usage,
	})
}

// MessageUsageHistorySnapshot returns a copy of the per-message usage
// records, the lock-safe read-side counterpart to AddMessageUsageRecord for
// callers on other goroutines (e.g. the TUI cost dialog).
func (s *Session) MessageUsageHistorySnapshot() []MessageUsageRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.MessageUsageHistory)
}

// AddAttachedFile records absPath as a file the user attached to this session.
// The path must be absolute; relative paths are silently dropped (with a debug
// log) since they would be ambiguous to sub-agents started in a fresh working
// directory. Empty paths and duplicates already present in AttachedFiles are
// also dropped.
//
// The recorded paths are propagated to sub-sessions created via task transfer
// so that delegated agents can read the same files without having to scan the
// workspace or guess from a bare filename.
func (s *Session) AddAttachedFile(absPath string) {
	if absPath == "" {
		return
	}
	if !filepath.IsAbs(absPath) {
		slog.Debug("ignoring non-absolute attached file path", "session_id", s.ID, "path", absPath)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.Contains(s.AttachedFiles, absPath) {
		return
	}
	s.AttachedFiles = append(s.AttachedFiles, absPath)
}

// RemoveAttachedFile removes absPath from the session's attached files and
// reports whether it was present. Removing a path only stops it from being
// propagated to future sub-agent delegations and skill prompts; file content
// already inlined in past messages is untouched.
func (s *Session) RemoveAttachedFile(absPath string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.Index(s.AttachedFiles, absPath)
	if i < 0 {
		return false
	}
	s.AttachedFiles = slices.Delete(s.AttachedFiles, i, i+1)
	return true
}

// AttachedFilesSnapshot returns a copy of the session's attached file paths.
// Callers may freely mutate the returned slice without affecting the session.
func (s *Session) AttachedFilesSnapshot() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.AttachedFiles)
}

// AttributesSnapshot returns an independent copy of the session attributes.
func (s *Session) AttributesSnapshot() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return maps.Clone(s.Attributes)
}

// SetAttribute sets a session attribute. Empty keys are ignored because they
// cannot form a meaningful namespaced metadata key.
func (s *Session) SetAttribute(key, value string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Attributes == nil {
		s.Attributes = make(map[string]string)
	}
	s.Attributes[key] = value
}

// DeleteAttribute deletes a session attribute. An empty key is a no-op,
// matching SetAttribute and WithAttributes.
func (s *Session) DeleteAttribute(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Attributes, key)
}

// DelegationLineageSnapshot returns a copy of the session's delegation
// lineage. Callers may freely mutate the returned slice without affecting
// the session.
func (s *Session) DelegationLineageSnapshot() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.DelegationLineage)
}

type Opt func(s *Session)

// WithAttributes sets generic session metadata from an independent copy of
// attributes. Empty keys are discarded; empty values are preserved.
func WithAttributes(attributes map[string]string) Opt {
	cloned := maps.Clone(attributes)
	delete(cloned, "")
	return func(s *Session) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.Attributes = maps.Clone(cloned)
	}
}

func WithUserMessage(content string) Opt {
	return func(s *Session) {
		s.AddMessage(UserMessageAt(s.now(), content))
	}
}

func WithImplicitUserMessage(content string) Opt {
	return func(s *Session) {
		s.AddMessage(ImplicitUserMessageAt(s.now(), content))
	}
}

func WithSystemMessage(content string) Opt {
	return func(s *Session) {
		s.AddMessage(SystemMessageAt(s.now(), content))
	}
}

func WithMaxIterations(maxIterations int) Opt {
	return func(s *Session) {
		s.MaxIterations = maxIterations
	}
}

// WithMaxConsecutiveToolCalls sets the threshold for consecutive identical tool
// call detection. 0 means "use runtime default of 5". Negative values are
// ignored.
func WithMaxConsecutiveToolCalls(n int) Opt {
	return func(s *Session) {
		if n >= 0 {
			s.MaxConsecutiveToolCalls = n
		}
	}
}

// WithMaxOldToolCallTokens sets the maximum token budget for old tool call content.
// Positive values enable truncation; 0 and -1 disable truncation (unlimited tool content).
func WithMaxOldToolCallTokens(n int) Opt {
	return func(s *Session) {
		s.MaxOldToolCallTokens = n
	}
}

// WithMaxToolResultTokens sets the per-tool-result token cap applied at
// ingestion. Positive values enable middle-out truncation; 0 and -1 disable
// the cap (unbounded tool results).
func WithMaxToolResultTokens(n int) Opt {
	return func(s *Session) {
		s.MaxToolResultTokens = n
	}
}

func WithOrigin(origin string) Opt {
	return func(s *Session) {
		s.Origin = origin
	}
}

func WithWorkingDir(workingDir string) Opt {
	return func(s *Session) {
		s.WorkingDir = workingDir
	}
}

func WithTitle(title string) Opt {
	return func(s *Session) {
		s.SetTitle(title)
	}
}

func WithMessages(messages []Item) Opt {
	return func(s *Session) {
		s.Messages = messages
	}
}

// WithToolsApproved is the legacy --yolo setter. Prefer
// [WithSafetyPolicy]. With toolsApproved=true and no explicit
// SafetyPolicy, pins the policy to [SafetyPolicyAutonomous].
func WithToolsApproved(toolsApproved bool) Opt {
	return func(s *Session) {
		s.ToolsApproved = toolsApproved
		if toolsApproved && s.SafetyPolicy == "" {
			s.SafetyPolicy = SafetyPolicyAutonomous
		}
	}
}

// WithSafetyPolicy sets the session's safety mode. The input is
// normalized (legacy aliases map onto the current mode vocabulary) and
// ToolsApproved is kept in sync so legacy readers of that flag agree
// with the mode. Empty means "no explicit choice" and is a no-op, so
// the option composes with [WithToolsApproved] regardless of order
// (--yolo callers pass ToolsApproved=true and an empty policy).
func WithSafetyPolicy(policy SafetyPolicy) Opt {
	return func(s *Session) {
		policy = policy.Normalize()
		if policy == "" {
			return
		}
		s.SafetyPolicy = policy
		s.ToolsApproved = policy == SafetyPolicyAutonomous
	}
}

func WithNonInteractive(nonInteractive bool) Opt {
	return func(s *Session) {
		s.NonInteractive = nonInteractive
	}
}

func WithHideToolResults(hideToolResults bool) Opt {
	return func(s *Session) {
		s.HideToolResults = hideToolResults
	}
}

func WithSendUserMessage(sendUserMessage bool) Opt {
	return func(s *Session) {
		s.SendUserMessage = sendUserMessage
	}
}

func WithPermissions(perms *PermissionsConfig) Opt {
	return func(s *Session) {
		s.Permissions = perms.Clone()
	}
}

// WithAgentName pins this session to a specific agent. When set, RunStream
// resolves the agent from the session rather than the shared runtime state,
// which is required for concurrent background agent tasks.
func WithAgentName(name string) Opt {
	return func(s *Session) {
		s.AgentName = name
	}
}

// WithParentID marks this session as a sub-session of the given parent.
// Sub-sessions are not persisted as standalone entries in the session store.
func WithParentID(parentID string) Opt {
	return func(s *Session) {
		s.ParentID = parentID
	}
}

// WithDelegationLineage records the chain of agents that delegated to produce
// this sub-session (root caller first, excluding the session's own agent).
// The slice is cloned so the session never shares backing storage with the
// caller — required for concurrent delegation fan-out from one parent.
func WithDelegationLineage(names []string) Opt {
	return func(s *Session) {
		s.DelegationLineage = slices.Clone(names)
	}
}

// WithID sets the session ID. If not set, a UUID will be generated.
func WithID(id string) Opt {
	return func(s *Session) {
		s.ID = id
	}
}

// WithClock injects the time source used for the session's CreatedAt, for
// timestamping messages it generates (summaries, compaction input), and for
// messages added through WithUserMessage/WithSystemMessage/
// WithImplicitUserMessage. Because those message options read the clock when
// they run, WithClock must precede them in the option list (the natural
// ordering). Primarily for tests that need a deterministic clock; production
// code leaves it unset and falls back to time.Now.
func WithClock(now func() time.Time) Opt {
	return func(s *Session) {
		s.clock = now
	}
}

// WithIDGen injects the generator used to mint the session ID when no explicit
// ID is provided. Primarily for tests that need deterministic IDs; production
// code leaves it unset and falls back to a random UUID.
func WithIDGen(gen func() string) Opt {
	return func(s *Session) {
		s.idgen = gen
	}
}

// WithExcludedTools sets tool names that should be filtered out of the agent's
// tool list for this session. This prevents recursive tool calls in skill
// sub-sessions.
func WithExcludedTools(names []string) Opt {
	return func(s *Session) {
		s.ExcludedTools = names
	}
}

// WithAllowedTools restricts the session's agent tools to those whose names
// match an entry (glob or exact). Used by fork-mode skill sub-sessions.
// ExtraToolSets are exempt from this filter.
func WithAllowedTools(names []string) Opt {
	return func(s *Session) {
		s.AllowedTools = names
	}
}

// WithExtraToolSets injects additional toolsets into the session on top of
// the agent's own toolsets. Used by fork-mode skill sub-sessions that declare
// assistive toolsets.
func WithExtraToolSets(toolSets []tools.ToolSet) Opt {
	return func(s *Session) {
		s.ExtraToolSets = toolSets
	}
}

// WithStructuredOutputDisabled exempts the session from tool-mode
// structured-output enforcement. Used by fork-mode skill sub-sessions whose
// answers are free-form text for the calling agent.
func WithStructuredOutputDisabled(disabled bool) Opt {
	return func(s *Session) {
		s.DisableStructuredOutput = disabled
	}
}

// WithAttachedFiles seeds the session with absolute paths of files the user
// attached. Used when creating sub-sessions so that delegated agents inherit
// the parent's file context. Empty and duplicate paths are dropped.
func WithAttachedFiles(paths []string) Opt {
	return func(s *Session) {
		for _, p := range paths {
			s.AddAttachedFile(p)
		}
	}
}

// IsSubSession returns true if this session is a sub-session (has a parent).
func (s *Session) IsSubSession() bool {
	return s.ParentID != ""
}

// MessageCount returns the number of items that contain a message.
func (s *Session) MessageCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	n := 0
	for _, item := range s.Messages {
		if item.IsMessage() {
			n++
		}
	}
	return n
}

// ItemCount returns the total number of items in s.Messages — messages,
// sub-sessions, summaries, and recorded errors alike. Unlike MessageCount,
// it counts every item, matching what len(s.Messages) would return outside
// the lock. Hot paths that need "the index the next appended item will
// occupy" (e.g. before calling AddSubSession) should use this instead of
// reading len(sess.Messages) directly, which races with concurrent
// AddMessage/ApplyCompaction.
func (s *Session) ItemCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.Messages)
}

// MessagesSnapshot returns a lock-safe copy of the session's items, deep-
// copying each Message so the result cannot alias a concurrent AddMessage /
// UpdateMessage mutation. It is the exported counterpart of snapshotItems for
// callers outside this package (e.g. pkg/server's ForkSession) that need to
// iterate Messages without racing session.mu; in-package callers should keep
// using snapshotItems directly.
func (s *Session) MessagesSnapshot() []Item {
	return s.snapshotItems()
}

// TotalCost computes the total cost of a session by walking all messages,
// sub-sessions, and summary items. It does not use the session-level Cost
// field, which exists only for backward-compatible persistence.
func (s *Session) TotalCost() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var cost float64
	for _, item := range s.Messages {
		switch {
		case item.IsMessage():
			cost += item.Message.Message.Cost
		case item.IsSubSession():
			cost += item.SubSession.TotalCost()
		}
		cost += item.Cost
	}
	return cost
}

// OwnCost returns only this session's direct cost: its own messages and
// item-level costs (e.g. compaction). It excludes sub-session costs.
// This is used for live event emissions where sub-sessions report their
// own costs separately.
func (s *Session) OwnCost() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var cost float64
	for _, item := range s.Messages {
		if item.IsMessage() {
			cost += item.Message.Message.Cost
		}
		cost += item.Cost
	}
	return cost
}

// EmbeddedSubSessionCost returns the total cost of sub-sessions that were
// already embedded when this session was loaded (restored or branched
// history), excluding sub-sessions attached live via AddLiveSubSession.
// Embedded sub-sessions never emit their own TokenUsageEvents, so live
// cost reporting must fold their cost into this session's own cost;
// live sub-sessions report theirs through separate per-session events.
func (s *Session) EmbeddedSubSessionCost() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var cost float64
	for _, item := range s.Messages {
		if item.IsSubSession() && !item.liveAttached {
			cost += item.SubSession.TotalCost()
		}
	}
	return cost
}

// IsToolsApproved reports whether every tool call auto-approves. It is
// derived from the effective safety mode — [SafetyPolicyAutonomous] —
// so a mode downgrade takes effect even if the legacy ToolsApproved
// flag was left behind by an older writer. Safe for concurrent use.
func (s *Session) IsToolsApproved() bool {
	return s.GetSafetyPolicy() == SafetyPolicyAutonomous
}

// GetSafetyPolicy returns the session's effective safety mode: the
// normalized explicit policy or, when none was ever chosen,
// [SafetyPolicyAutonomous] if the legacy ToolsApproved flag is set and
// the empty legacy default otherwise. Safe for concurrent use.
func (s *Session) GetSafetyPolicy() SafetyPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	policy := s.SafetyPolicy.Normalize()
	if policy == "" && s.ToolsApproved {
		return SafetyPolicyAutonomous
	}
	return policy
}

// ClonePermissions returns a deep copy of the session's PermissionsConfig.
// This is safe to call concurrently with session mutations.
func (s *Session) ClonePermissions() *PermissionsConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Permissions.Clone()
}

// SetPermissions safely updates the session's PermissionsConfig.
func (s *Session) SetPermissions(perms *PermissionsConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Permissions = perms
}

// SetToolsApproved is the legacy --yolo toggle. Prefer
// [Session.SetSafetyPolicy]; this maps true → Autonomous (when no
// explicit mode was chosen) so the two signals cannot disagree.
func (s *Session) SetToolsApproved(approved bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ToolsApproved = approved
	if approved && s.SafetyPolicy == "" {
		s.SafetyPolicy = SafetyPolicyAutonomous
	}
}

// SetSafetyPolicy updates the session's safety mode under s.mu,
// normalizing legacy aliases. ToolsApproved is synced in the same
// critical section (Autonomous → true, anything else → false) so a
// mode downgrade genuinely revokes the blanket approval, serialized
// state stays coherent for legacy readers, and no concurrent
// GetSafetyPolicy can observe a half-updated combination.
//
// SetSafetyPolicy("") is NOT a no-op: it is a full reset to the legacy
// default (read-only tools auto-approve, everything else asks) and
// CLEARS ToolsApproved, demoting a --yolo session. Callers that want
// to keep a blanket approval must pass [SafetyPolicyAutonomous]; the
// [WithSafetyPolicy] option is the empty-means-unset variant.
//
// Runtime callers use this to persist a user's mid-session mode change
// (e.g. opting into Balanced from a confirmation prompt).
func (s *Session) SetSafetyPolicy(policy SafetyPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SafetyPolicy = policy.Normalize()
	s.ToolsApproved = s.SafetyPolicy == SafetyPolicyAutonomous
	// An explicit choice invalidates the yolo-toggle memory: toggling
	// off later must not resurrect a mode the user has since replaced.
	s.PriorSafetyPolicy = ""
}

// GetPriorSafetyPolicy returns the yolo-toggle memory (see
// [Session.ToggleYolo]). Safe for concurrent use.
func (s *Session) GetPriorSafetyPolicy() SafetyPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.PriorSafetyPolicy
}

// ToggleYolo flips the session between Autonomous and the mode that was
// active before the escalation, so an explicit Balanced/Strict choice
// survives a toggle round-trip. A session that was never escalated from
// a named mode drops back to the legacy default (""). The method is its
// own inverse, which callers use to roll back a toggle whose
// persistence failed.
func (s *Session) ToggleYolo() {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.SafetyPolicy.Normalize()
	if current == "" && s.ToolsApproved {
		current = SafetyPolicyAutonomous
	}
	if current == SafetyPolicyAutonomous {
		s.SafetyPolicy = s.PriorSafetyPolicy.Normalize()
		s.PriorSafetyPolicy = ""
	} else {
		s.PriorSafetyPolicy = current
		s.SafetyPolicy = SafetyPolicyAutonomous
	}
	s.ToolsApproved = s.SafetyPolicy == SafetyPolicyAutonomous
}

// AppendPermissionAllow adds toolName to the session's Allow list if not
// already present, initializing Permissions when nil. Guarded by s.mu so
// concurrent readers (ClonePermissions, permissionCheckers) see a
// consistent snapshot.
func (s *Session) AppendPermissionAllow(toolName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Permissions == nil {
		s.Permissions = &PermissionsConfig{}
	}
	if !slices.Contains(s.Permissions.Allow, toolName) {
		s.Permissions.Allow = append(s.Permissions.Allow, toolName)
	}
}

// now returns the session's current time, falling back to time.Now for
// sessions created without a clock (e.g. JSON deserialization).
func (s *Session) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// newID returns a fresh session ID using the session's generator, falling back
// to a random UUID for sessions created without one.
func (s *Session) newID() string {
	if s.idgen != nil {
		return s.idgen()
	}
	return defaultNewID()
}

// New creates a new agent session
func New(opts ...Opt) *Session {
	s := &Session{
		Origin:          "run",
		SendUserMessage: true,
	}

	for _, opt := range opts {
		opt(s)
	}

	// Generate the ID and creation time after options run so that
	// WithClock/WithIDGen (and WithID) can influence them. WithID short-
	// circuits the generator when an explicit ID was provided.
	if s.ID == "" {
		s.ID = s.newID()
	}
	s.CreatedAt = s.now()

	slog.Debug("Creating new session", "session_id", s.ID)
	return s
}

func markLastMessageAsCacheControl(messages []chat.Message) {
	if len(messages) > 0 {
		messages[len(messages)-1].CacheControl = true
	}
}

// buildInvariantSystemMessages builds system messages that are identical
// for all users of a given agent configuration. These messages can be
// cached efficiently as they don't change between sessions, users, or projects.
//
// These messages are determined solely by the agent configuration and
// remain constant across different sessions, users, and working directories.
func buildInvariantSystemMessages(a *agent.Agent) []chat.Message {
	var messages []chat.Message

	if a.HasSubAgents() {
		subAgents := a.SubAgents()

		var text strings.Builder
		var validAgentIDs []string
		for _, subAgent := range subAgents {
			text.WriteString("Name: ")
			text.WriteString(subAgent.Name())
			text.WriteString(" | Description: ")
			text.WriteString(subAgent.Description())
			text.WriteString("\n")

			validAgentIDs = append(validAgentIDs, subAgent.Name())
		}

		messages = append(messages, chat.Message{
			Role:    chat.MessageRoleSystem,
			Content: "You are a multi-agent system, make sure to answer the user query in the most helpful way possible. You have access to these sub-agents:\n" + text.String() + "\nIMPORTANT: You can ONLY transfer tasks to the agents listed above using their ID. The valid agent names are: " + strings.Join(validAgentIDs, ", ") + ". You MUST NOT attempt to transfer to any other agent IDs - doing so will cause system errors.\n\nIf you are the best to answer the question according to your description, you can answer it.\n\nIf another agent is better for answering the question according to its description, call `transfer_task` function to transfer the question to that agent using the agent's ID. When transferring, do not generate any text other than the function call.\n\nWhen the task involves files, always include their absolute paths in the `task` description (never just bare filenames). Sub-agents start in a fresh session and do not see the conversation history or files attached by the user, so a non-absolute path may resolve to the wrong file or force the sub-agent to scan the filesystem.\n\n",
		})
	}

	if handoffs := a.Handoffs(); len(handoffs) > 0 {
		var text strings.Builder
		var validAgentIDs []string
		for _, agent := range handoffs {
			text.WriteString("Name: ")
			text.WriteString(agent.Name())
			text.WriteString(" | Description: ")
			text.WriteString(agent.Description())
			text.WriteString("\n")

			validAgentIDs = append(validAgentIDs, agent.Name())
		}

		handoffPrompt := "You are part of a multi-agent team. Your goal is to answer the user query in the most helpful way possible.\n\n" +
			"Available agents in your team:\n" + text.String() + "\n" +
			"You can hand off the conversation to any of these agents at any time by using the `handoff` function with their ID. " +
			"The valid agent IDs are: " + strings.Join(validAgentIDs, ", ") + ".\n\n" +
			"When to hand off:\n" +
			"- If another agent's description indicates they are better suited for the current task or question\n" +
			"- If the user explicitly asks for a specific agent\n" +
			"- If you need specialized capabilities that another agent provides\n\n" +
			"If you are the best agent to handle the current request based on your capabilities, respond directly. " +
			"When handing off to another agent, only handoff without talking about the handoff."

		messages = append(messages, chat.Message{
			Role:    chat.MessageRoleSystem,
			Content: handoffPrompt,
		})
	}

	if instructions := a.Instruction(); instructions != "" {
		messages = append(messages, chat.Message{
			Role:    chat.MessageRoleSystem,
			Content: instructions,
		})
	}

	for _, toolSet := range a.ToolSets() {
		if instructions := tools.GetInstructions(toolSet); instructions != "" {
			messages = append(messages, chat.Message{
				Role:    chat.MessageRoleSystem,
				Content: instructions,
			})
		}
	}

	return messages
}

// summaryMessagePrefix prefixes the synthetic user message that carries a
// compaction summary into the prompt. Shared by buildSessionSummaryMessages
// and CompactionInput; exposed to callers via SummaryMessageContent.
const summaryMessagePrefix = "Session Summary: "

// SummaryMessageContent returns the content of the synthetic user message
// that GetMessages emits to carry a compaction summary into the prompt.
// Callers that need to recognize that message in GetMessages output (e.g.
// the runtime's context-window breakdown) match against this exact string
// instead of duplicating the prefix.
func SummaryMessageContent(summary string) string {
	return summaryMessagePrefix + summary
}

// LastSummary returns the most recent compaction summary stored in the
// session history, or "" when the session has never been compacted.
func (s *Session) LastSummary() string {
	items := s.snapshotItems()
	for i := range slices.Backward(items) {
		if items[i].Summary != "" {
			return items[i].Summary
		}
	}
	return ""
}

// buildSessionSummaryMessages builds system messages containing the session summary
// if one exists. Session summaries are context-specific per session and thus should not have a checkpoint (they will be cached alongside the first user message anyway)
//
// startIndex is the index in items from which conversation messages should be
// emitted. When a summary with FirstKeptEntry is present, this points to the
// first kept message so that recent context is preserved after compaction.
// Otherwise it is lastSummaryIndex+1 (i.e. right after the summary item), or
// 0 when there is no summary.
//
// summary is the raw text of the last summary item in items ("" when there
// is none), i.e. exactly the text the synthetic message carries.
func (s *Session) buildSessionSummaryMessages(items []Item) ([]chat.Message, int, string) {
	var messages []chat.Message
	// Find the last summary index to determine where conversation messages start
	// and to include the summary in session summary messages
	lastSummaryIndex := -1
	for i := range slices.Backward(items) {
		if items[i].Summary != "" {
			lastSummaryIndex = i
			break
		}
	}

	summary := ""
	if lastSummaryIndex >= 0 && lastSummaryIndex < len(items) {
		summary = items[lastSummaryIndex].Summary
		messages = append(messages, chat.Message{
			Role:      chat.MessageRoleUser,
			Content:   SummaryMessageContent(summary),
			CreatedAt: s.now().Format(time.RFC3339),
		})
	}

	// Determine where conversation messages should start.
	// If the summary has a FirstKeptEntry, we start from there so that
	// messages kept during compaction are included after the summary.
	startIndex := lastSummaryIndex + 1
	if lastSummaryIndex >= 0 {
		kept := items[lastSummaryIndex].FirstKeptEntry
		if kept > 0 && kept < lastSummaryIndex {
			startIndex = kept
		}
	}

	return messages, startIndex, summary
}

// CompactionInput returns the chat messages that the compactor should
// summarize together with their origin indices in s.Messages. The
// returned messages are independent copies safe for the caller to
// mutate (cloned via snapshotItems); the parallel sessIndices slice
// maps each entry back to its source item so the caller can compute a
// FirstKeptEntry that survives prior summaries in the history.
//
// When the session contains a prior summary, the result begins with a
// synthetic "Session Summary: ..." user message whose origin index is
// the prior summary item itself; subsequent entries are the prior
// kept-tail and the post-summary conversation, mirroring what
// buildSessionSummaryMessages produces for the runtime. System
// messages stored on the session are filtered out (the compactor
// supplies its own system/user prompt around this list).
//
// This method intentionally bypasses GetMessages's agent-level
// transformations — invariant system prompts, NumHistoryItems
// trimming, old-tool-content truncation, whitespace normalization,
// orphan-tool-call sanitization, and cache_control marking. None of
// those belong in compaction input: the compactor needs the full,
// untrimmed history (so the LLM can summarize what trimming would
// have hidden), supplies its own system/user prompt, and runs through
// a sub-runtime that re-applies sanitization on its own session.
//
// The third return value is the snapshot's total item count
// (len(s.Messages) at the instant the snapshot was taken). Callers that
// need an out-of-range sentinel for a split computed against messages/
// sessIndices (i.e. "keep nothing of the tail") must use this value rather
// than a fresh call to ItemCount(): the live count can already include an
// append that happened after this snapshot, which would describe a longer
// session than the one messages/sessIndices actually cover.
//
// All work is performed under s.mu.RLock via snapshotItems, so this
// method is safe to call concurrently with AddMessage / ApplyCompaction
// on the same session.
func (s *Session) CompactionInput() ([]chat.Message, []int, int) {
	items := s.snapshotItems()

	lastSummaryIndex := -1
	for i := range slices.Backward(items) {
		if items[i].Summary != "" {
			lastSummaryIndex = i
			break
		}
	}

	var (
		messages    []chat.Message
		sessIndices []int
	)

	if lastSummaryIndex >= 0 {
		messages = append(messages, chat.Message{
			Role:      chat.MessageRoleUser,
			Content:   SummaryMessageContent(items[lastSummaryIndex].Summary),
			CreatedAt: s.now().Format(time.RFC3339),
		})
		// The synthetic message stands in for the prior summary item;
		// when this index lands inside the kept tail we want the
		// summary item itself preserved so the next compaction round
		// still sees it via buildSessionSummaryMessages.
		sessIndices = append(sessIndices, lastSummaryIndex)
	}

	startIndex := lastSummaryIndex + 1
	if lastSummaryIndex >= 0 {
		kept := items[lastSummaryIndex].FirstKeptEntry
		if kept > 0 && kept < lastSummaryIndex {
			startIndex = kept
		}
	}

	for i := startIndex; i < len(items); i++ {
		if !items[i].IsMessage() {
			continue
		}
		msg := items[i].Message.Message
		if msg.Role == chat.MessageRoleSystem {
			continue
		}
		// Clear per-request bookkeeping on the copy: callers feed these
		// messages into a new LLM request, so per-message Cost (already
		// folded into the session totals) would double-count, and
		// CacheControl marks (request-assembly state that legacy persisted
		// rows may still carry) would pin a provider cache checkpoint on
		// the wrong request.
		msg.Cost = 0
		msg.CacheControl = false
		messages = append(messages, msg)
		sessIndices = append(sessIndices, i)
	}
	return messages, sessIndices, len(items)
}

// ClearInstructionContext removes a previously prepared cache-stable snapshot.
// It returns whether persisted session metadata changed.
func (s *Session) ClearInstructionContext() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.InstructionContext == nil {
		return false
	}
	s.InstructionContext = nil
	return true
}

func (s *Session) instructionMessages() ([]chat.Message, []InstructionUpdate) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.InstructionContext == nil {
		return nil, nil
	}

	initial := make([]chat.Message, 0, len(s.InstructionContext.Initial))
	for _, key := range s.InstructionContext.Order {
		value, ok := s.InstructionContext.Initial[key]
		if !ok || strings.TrimSpace(value.Content) == "" {
			continue
		}
		initial = append(initial, chat.Message{Role: chat.MessageRoleSystem, Content: value.Content})
	}
	return initial, slices.Clone(s.InstructionContext.Updates)
}

func (s *Session) GetMessages(a *agent.Agent, extraSystemMessages ...chat.Message) []chat.Message {
	messages, _ := s.getMessages(a, true, extraSystemMessages...)
	return messages
}

// GetMessagesWithoutInstructionContext assembles the legacy prompt where
// dynamic context is supplied directly as extra system messages.
func (s *Session) GetMessagesWithoutInstructionContext(a *agent.Agent, extraSystemMessages ...chat.Message) []chat.Message {
	messages, _ := s.getMessages(a, false, extraSystemMessages...)
	return messages
}

// GetMessagesAndLastSummary is GetMessages plus the most recent compaction
// summary that assembly used, both derived from the same snapshot of
// s.Messages: the returned summary is exactly the text behind the synthetic
// "Session Summary: ..." user message in the returned prompt ("" when the
// snapshot holds no summary), even if a compaction lands concurrently.
// Separate GetMessages and LastSummary calls cannot promise that. The
// guarantee covers only the session-history snapshot, not the other state
// read during assembly (instruction context, agent configuration).
func (s *Session) GetMessagesAndLastSummary(a *agent.Agent, extraSystemMessages ...chat.Message) ([]chat.Message, string) {
	return s.getMessages(a, true, extraSystemMessages...)
}

func (s *Session) getMessages(a *agent.Agent, includeInstructionContext bool, extraSystemMessages ...chat.Message) ([]chat.Message, string) {
	slog.Debug("Getting messages for agent", "agent", a.Name(), "session_id", s.ID)

	// Build invariant system messages (cacheable across sessions/users/projects)
	invariantMessages := buildInvariantSystemMessages(a)
	markLastMessageAsCacheControl(invariantMessages)

	// Take a snapshot of Messages under the lock, copying Message structs
	// to avoid racing with UpdateMessage which may modify the pointed-to objects.
	items := s.snapshotItems()
	var instructionInitial []chat.Message
	var instructionUpdates []InstructionUpdate
	if includeInstructionContext {
		instructionInitial, instructionUpdates = s.instructionMessages()
	}

	// Build session summary messages (vary per session)
	summaryMessages, startIndex, summary := s.buildSessionSummaryMessages(items)

	var messages []chat.Message
	messages = append(messages, invariantMessages...)
	messages = append(messages, instructionInitial...)
	markLastMessageAsCacheControl(messages)
	// extraSystemMessages are caller-supplied transient system messages
	// (e.g. turn_start hook output) inserted after the invariant cache
	// checkpoint and before the conversation. The last extra carries a
	// cache_control marker so that stable per-session/per-day extras
	// (AddPromptFiles, AddEnvironmentInfo) participate in prompt caching.
	// Volatile extras (the daily date) live behind the same marker, which
	// is acceptable: the cache simply rotates when the date rolls over,
	// matching the behavior of the previous inline
	// buildContextSpecificSystemMessages path.
	if len(extraSystemMessages) > 0 {
		messages = append(messages, extraSystemMessages...)
		markLastMessageAsCacheControl(messages[len(messages)-len(extraSystemMessages):])
	}
	messages = append(messages, summaryMessages...)

	// Begin adding conversation messages, interleaving instruction changes at
	// the history position where they were observed.
	updateIndex := 0
	for updateIndex < len(instructionUpdates) && instructionUpdates[updateIndex].Position < startIndex {
		updateIndex++
	}
	for i := startIndex; i <= len(items); i++ {
		for updateIndex < len(instructionUpdates) && instructionUpdates[updateIndex].Position == i {
			messages = append(messages, chat.Message{
				Role:    chat.MessageRoleUser,
				Content: instructionUpdates[updateIndex].Content,
			})
			updateIndex++
		}
		if i < len(items) && items[i].IsMessage() {
			messages = append(messages, items[i].Message.Message)
		}
	}

	maxItems := a.NumHistoryItems()
	if maxItems > 0 {
		messages = trimMessages(messages, maxItems)
	}

	// Truncation of old tool-call content is opt-in: only a positive token
	// budget truncates. 0 (unset/omitted) and -1 both disable truncation.
	if s.MaxOldToolCallTokens > 0 {
		messages = truncateOldToolContent(messages, s.MaxOldToolCallTokens)
	}

	messages = normalizeMessageContent(messages)
	messages = sanitizeToolCalls(messages)

	// Read-time backstop: tool results that entered the history without
	// passing through AddMessage (e.g. persisted via the API/SQLite path and
	// reloaded) may still be unbounded, so reapply the cap to the assembled
	// messages just before they reach a provider. These are snapshot copies
	// (snapshotItems), so the stored history is never mutated by a read.
	if s.MaxToolResultTokens > 0 {
		for i := range messages {
			capToolResultContent(&messages[i], s.MaxToolResultTokens)
		}
	}

	systemCount := 0
	conversationCount := 0
	for i := range messages {
		if messages[i].Role == chat.MessageRoleSystem {
			systemCount++
		} else {
			conversationCount++
		}
	}

	slog.Debug("Retrieved messages for agent",
		"agent", a.Name(),
		"session_id", s.ID,
		"total_messages", len(messages),
		"system_messages", systemCount,
		"conversation_messages", conversationCount,
		"max_history_items", maxItems)

	return messages, summary
}

// trimMessages ensures we don't exceed the maximum number of messages while maintaining
// consistency between assistant messages and their tool call results.
// System messages and user messages are always preserved and not counted against the limit.
// User messages are protected from trimming to prevent the model from losing
// track of what was asked in long agentic loops.
func trimMessages(messages []chat.Message, maxItems int) []chat.Message {
	// Separate system messages from conversation messages
	var systemMessages []chat.Message
	var conversationMessages []chat.Message

	for i := range messages {
		if messages[i].Role == chat.MessageRoleSystem {
			systemMessages = append(systemMessages, messages[i])
		} else {
			conversationMessages = append(conversationMessages, messages[i])
		}
	}

	// If conversation messages fit within limit, return all messages
	if len(conversationMessages) <= maxItems {
		return messages
	}

	// Identify user message indices — these are protected from trimming
	protected := make(map[int]bool)
	for i, msg := range conversationMessages {
		if msg.Role == chat.MessageRoleUser {
			protected[i] = true
		}
	}

	// Keep track of tool call IDs that need to be removed
	toolCallsToRemove := make(map[string]bool)

	// Calculate how many conversation messages we need to remove
	toRemove := len(conversationMessages) - maxItems

	// Mark the oldest non-protected messages for removal
	removed := make(map[int]bool)
	for i := 0; i < len(conversationMessages) && len(removed) < toRemove; i++ {
		if protected[i] {
			continue
		}
		removed[i] = true
		if conversationMessages[i].Role == chat.MessageRoleAssistant {
			for _, toolCall := range conversationMessages[i].ToolCalls {
				toolCallsToRemove[toolCall.ID] = true
			}
		}
	}

	// Combine system messages with trimmed conversation messages
	result := make([]chat.Message, 0, len(systemMessages)+maxItems)

	// Add all system messages first
	result = append(result, systemMessages...)

	// Add protected and non-removed conversation messages
	for i, msg := range conversationMessages {
		if removed[i] {
			continue
		}

		// Skip orphaned tool results whose assistant message was removed
		if msg.Role == chat.MessageRoleTool && toolCallsToRemove[msg.ToolCallID] {
			continue
		}

		result = append(result, msg)
	}

	return result
}

// normalizeMessageContent strips purely-whitespace content from messages before
// they reach any provider converter. Specifically:
//
//   - Non-tool messages whose Content is whitespace-only and have no MultiContent
//     are dropped entirely. Tool-result messages are exempt: every tool_use must
//     have a corresponding tool_result, so we cannot skip them even when empty.
//   - Text parts inside MultiContent whose Text is whitespace-only are removed.
//     A non-tool message that becomes part-less after this pruning is also dropped.
//
// This is the single authoritative guard; individual provider converters do not
// need their own whitespace-skip guards for user/system/assistant messages.
func normalizeMessageContent(messages []chat.Message) []chat.Message {
	out := messages[:0:0]          // reuse underlying array, length 0
	for _, msg := range messages { // Tool results must always be forwarded — even empty — because the API
		// requires a tool_result for every preceding tool_use block.
		if msg.Role == chat.MessageRoleTool {
			out = append(out, msg)
			continue
		}

		if len(msg.MultiContent) > 0 {
			// Filter whitespace-only text parts; preserve image/file parts as-is.
			filtered := msg.MultiContent[:0:0]
			for _, part := range msg.MultiContent {
				if part.Type == chat.MessagePartTypeText && strings.TrimSpace(part.Text) == "" {
					continue
				}
				filtered = append(filtered, part)
			}
			if len(filtered) == 0 {
				// All parts were whitespace-only text — drop the whole message.
				continue
			}
			msg.MultiContent = filtered
			out = append(out, msg)
			continue
		}

		// Single-part: drop messages with whitespace-only Content, but only when
		// there are no tool calls or function calls attached. An assistant message
		// with an empty text body but tool_use blocks is valid and must be kept.
		if strings.TrimSpace(msg.Content) == "" && len(msg.ToolCalls) == 0 && msg.FunctionCall == nil {
			continue
		}
		out = append(out, msg)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sanitizeToolCalls ensures the tool_use/tool_result blocks sent to the
// provider are always balanced, in both directions:
//
//   - Every tool call in an assistant message gets a corresponding tool-result
//     message. It walks the message list tracking pending tool calls; when a
//     tool-result message arrives its ID is marked fulfilled. When the next
//     assistant or user message is encountered (or the end of the list is
//     reached), any still-pending tool calls receive synthetic error results
//     injected just before that boundary.
//   - Every tool-result message has a matching tool_use in the preceding
//     assistant message. Orphaned tool results — a tool-result whose
//     ToolCallID was never issued by the preceding assistant message — are
//     dropped. This happens when compaction's kept-tail boundary lands between
//     an assistant tool_use and its result, leaving the result at the head of
//     the resumed history with no matching tool_use. Providers such as AWS
//     Bedrock reject the request outright in that case ("The number of
//     toolResult blocks ... exceeds the number of toolUse blocks of previous
//     turn"), so we must strip these before the request goes out.
func sanitizeToolCalls(messages []chat.Message) []chat.Message {
	var (
		out              []chat.Message
		pendingToolCalls []tools.ToolCall
		pendingIDs       = make(map[string]bool)
		resultIDs        = make(map[string]bool)
	)

	flushPending := func() {
		for _, tc := range pendingToolCalls {
			if tc.ID != "" && !resultIDs[tc.ID] {
				out = append(out, chat.Message{
					Role:       chat.MessageRoleTool,
					ToolCallID: tc.ID,
					Content:    "No result provided",
					IsError:    true,
				})
			}
		}
		pendingToolCalls = nil
		pendingIDs = make(map[string]bool)
		resultIDs = make(map[string]bool)
	}

	for _, msg := range messages {
		switch {
		case msg.Role == chat.MessageRoleTool:
			// Drop orphaned tool results: a tool_result with no matching
			// tool_use in the preceding assistant message violates the
			// provider contract.
			if msg.ToolCallID == "" || !pendingIDs[msg.ToolCallID] {
				continue
			}
			// Drop duplicate tool results: a second result for the same
			// tool_use leaves more toolResult than toolUse blocks, which strict
			// providers (AWS Bedrock) reject the same way as an orphaned result.
			if resultIDs[msg.ToolCallID] {
				continue
			}
			resultIDs[msg.ToolCallID] = true

		case msg.Role == chat.MessageRoleAssistant && len(msg.ToolCalls) > 0:
			flushPending()
			out = append(out, msg)
			pendingToolCalls = msg.ToolCalls
			for _, tc := range msg.ToolCalls {
				if tc.ID != "" {
					pendingIDs[tc.ID] = true
				}
			}
			continue

		case msg.Role == chat.MessageRoleUser || msg.Role == chat.MessageRoleAssistant:
			flushPending()
		}

		out = append(out, msg)
	}

	flushPending()
	return out
}

// approximateTokens returns a coarse token count for a string, using the
// industry rule-of-thumb of ~4 characters per token. The heuristic is good
// enough for budgeting tool-content truncation; we do not need provider-exact
// counts here. Centralised so tests can reason about budgets without
// hard-coding the divisor.
func approximateTokens(s string) int {
	return len(s) / 4
}

// truncateOldToolContent replaces tool results with placeholders for older
// messages that exceed the token budget. It processes messages from newest to
// oldest, keeping recent tool content intact while truncating older content
// once the budget is exhausted.
func truncateOldToolContent(messages []chat.Message, maxTokens int) []chat.Message {
	if len(messages) == 0 || maxTokens <= 0 {
		return messages
	}

	result := make([]chat.Message, len(messages))
	copy(result, messages)

	tokenBudget := maxTokens

	for i := range slices.Backward(result) {
		msg := &result[i]

		if msg.Role == chat.MessageRoleTool {
			tokens := approximateTokens(msg.Content)
			if tokenBudget >= tokens {
				tokenBudget -= tokens
			} else {
				msg.Content = toolContentPlaceholder
				tokenBudget = 0
			}
		}
	}

	return result
}

// capToolResultContent applies the opt-in MaxToolResultTokens middle-out cap
// to a tool-result message, mutating msg in place. A single result-wide
// budget covers every textual payload a provider can receive from the
// message — Content plus MultiContent text parts and inline-text documents —
// so a result carrying many documents stays bounded instead of claiming one
// full cap per document. The budget is consumed in order — Content first,
// then MultiContent parts left to right — so payloads past an exhausted
// budget keep their metadata but lose their text (no proportional sharing).
// The text part duplicating Content (see chat.BuildToolResultMultiContent)
// is rewritten in lockstep and charged only once so providers cannot receive
// the unbounded copy. Document names, MIME types, and non-text parts are
// preserved; a truncated document's Size is updated to match the kept text.
//
// AddMessage applies the cap at ingestion (the primary path); GetMessages
// reapplies it to its local copies as a read-time backstop for results that
// entered the history without passing through AddMessage.
func capToolResultContent(msg *chat.Message, maxTokens int) {
	if maxTokens <= 0 || msg.Role != chat.MessageRoleTool {
		return
	}
	// Results whose aggregate textual payload approximates within the cap
	// stay untouched — this also makes the cap idempotent, so the read-time
	// reapply is a no-op for results already capped at ingestion. Past this
	// branch maxTokens*4 < aggregate bytes, so the byte conversions below
	// cannot overflow.
	if toolResultTextualBytes(msg)/4 <= maxTokens {
		return
	}

	remaining := maxTokens * 4

	original := msg.Content
	msg.Content = truncateMiddleOut(original, maxTokens)
	// Content may keep up to 3 bytes of len/4 rounding slack beyond the byte
	// budget; the aggregate still approximates within the cap.
	remaining = max(remaining-len(msg.Content), 0)

	fit := func(text string) string {
		kept := truncateMiddleOutBytes(text, remaining)
		remaining -= len(kept)
		return kept
	}
	contentDuplicateSynced := false
	for i := range msg.MultiContent {
		part := &msg.MultiContent[i]
		switch {
		case part.Type == chat.MessagePartTypeText && !contentDuplicateSynced && part.Text == original:
			// The first matching text part is the duplicate emitted by
			// BuildToolResultMultiContent. Rewrite it in lockstep and charge
			// it once via Content above; later identical parts are distinct.
			part.Text = msg.Content
			contentDuplicateSynced = true
		case part.Type == chat.MessagePartTypeText:
			part.Text = fit(part.Text)
		case part.Type == chat.MessagePartTypeDocument && part.Document != nil && part.Document.Source.InlineText != "":
			kept := fit(part.Document.Source.InlineText)
			if kept == part.Document.Source.InlineText {
				continue
			}
			// Copy-on-write so a Document shared with the caller is not
			// mutated behind its back.
			doc := *part.Document
			doc.Source.InlineText = kept
			doc.Size = int64(len(kept))
			part.Document = &doc
		}
	}
}

// toolResultTextualBytes sums the bytes of every textual payload a provider
// can receive from a tool-result message: Content, MultiContent text parts,
// and inline-text documents. The first text part equal to Content is the
// duplicate emitted by chat.BuildToolResultMultiContent — providers send
// either Content or the parts, never both — so it is counted once. Any later
// identical text parts are distinct payloads and still consume budget.
func toolResultTextualBytes(msg *chat.Message) int {
	total := len(msg.Content)
	contentDuplicateSeen := false
	for i := range msg.MultiContent {
		part := &msg.MultiContent[i]
		switch part.Type {
		case chat.MessagePartTypeText:
			if !contentDuplicateSeen && part.Text == msg.Content {
				contentDuplicateSeen = true
			} else {
				total += len(part.Text)
			}
		case chat.MessagePartTypeDocument:
			if part.Document != nil {
				total += len(part.Document.Source.InlineText)
			}
		}
	}
	return total
}

// truncateMiddleOut caps content at maxTokens approximate tokens
// (approximateTokens, len/4) by keeping the head and the tail and dropping
// the middle. Content already within the cap is returned unchanged.
func truncateMiddleOut(content string, maxTokens int) string {
	if maxTokens <= 0 || approximateTokens(content) <= maxTokens {
		return content
	}
	// Reaching here implies maxTokens < len(content)/4, so the byte budget
	// cannot overflow.
	return truncateMiddleOutBytes(content, maxTokens*4)
}

// truncateMiddleOutBytes keeps content within budget bytes by preserving the
// head and the tail and dropping the middle. Content within the budget is
// returned unchanged; a truncated result is always strictly smaller than the
// input, stays valid UTF-8 (cut points snap to rune starts), and flags the
// removed middle as clearly as the budget allows:
//
//  1. toolResultTruncationMarker between head and tail, when the budget fits
//     the marker plus at least one complete rune on each side;
//  2. a bare "..." between whatever head/tail still fits, for budgets too
//     small for the full marker;
//  3. the empty string when not even "..." fits — any shorter fragment would
//     read as complete output rather than as an elision.
func truncateMiddleOutBytes(content string, budget int) string {
	if len(content) <= budget {
		return content
	}

	if keep := budget - len(toolResultTruncationMarker); keep >= 2 {
		head, tailStart := middleOutCut(content, keep)
		if head > 0 && tailStart < len(content) {
			return content[:head] + toolResultTruncationMarker + content[tailStart:]
		}
	}

	const ellipsis = "..."
	keep := budget - len(ellipsis)
	if keep < 0 {
		return ""
	}
	head, tailStart := middleOutCut(content, keep)
	return content[:head] + ellipsis + content[tailStart:]
}

// middleOutCut splits a keep-byte allowance between a head prefix and a tail
// suffix of content, snapping both cut points to rune starts so the kept
// pieces stay valid UTF-8. Snapping only shrinks the pieces, so
// head + len(content) - tailStart never exceeds keep (either piece may snap
// to empty). Callers must ensure 0 <= keep < len(content).
func middleOutCut(content string, keep int) (head, tailStart int) {
	head = (keep + 1) / 2
	for head > 0 && !utf8.RuneStart(content[head]) {
		head--
	}
	tailStart = len(content) - keep/2
	for tailStart < len(content) && !utf8.RuneStart(content[tailStart]) {
		tailStart++
	}
	return head, tailStart
}
