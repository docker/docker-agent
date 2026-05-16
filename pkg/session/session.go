package session

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// nowFn returns the current time. Indirected through a package-level variable
// so that tests can install a deterministic clock via setNowForTest.
var nowFn = time.Now

// newIDFn returns a fresh session ID. Indirected through a package-level
// variable so that tests can install a deterministic ID generator via
// setIDForTest.
var newIDFn = func() string { return uuid.New().String() }

const (
	// DefaultMaxOldToolCallTokens is the default maximum number of tokens to keep from tool call
	// arguments and results. Older tool calls beyond this budget will have their
	// content replaced with a placeholder. Tokens are approximated by
	// approximateTokens (len/4).
	DefaultMaxOldToolCallTokens = 40000

	// toolContentPlaceholder is the text used to replace truncated tool content
	toolContentPlaceholder = "[content truncated]"
)

// Item represents either a message or a sub-session
type Item struct {
	// Message holds a regular conversation message
	Message *Message `json:"message,omitempty"`

	// SubSession holds a complete sub-session from task transfers
	SubSession *Session `json:"sub_session,omitempty"`

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
}

// IsMessage returns true if this item contains a message
func (si *Item) IsMessage() bool {
	return si.Message != nil
}

// IsSubSession returns true if this item contains a sub-session
func (si *Item) IsSubSession() bool {
	return si.SubSession != nil
}

// Session represents the agent's state including conversation history and variables
type Session struct {
	// mu protects Messages from concurrent read/write access.
	mu sync.RWMutex `json:"-"`

	// ID is the unique identifier for the session
	ID string `json:"id"`

	// Title is the title of the session, set by the runtime
	Title string `json:"title"`

	// Evals contains evaluation criteria for this session (used by eval framework)
	Evals *EvalCriteria `json:"evals,omitempty"`

	// EvalResult contains the evaluation scoring outcome (populated after eval run).
	EvalResult *EvalResult `json:"eval_result,omitempty"`

	// Messages holds the conversation history (messages and sub-sessions)
	Messages []Item `json:"messages"`

	// CreatedAt is the time the session was created
	CreatedAt time.Time `json:"created_at"`

	// ToolsApproved is a flag to indicate if the tools have been approved
	ToolsApproved bool `json:"tools_approved"`

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
	// approximateTokens (len/4).
	// Set to -1 to disable truncation (unlimited tool content).
	// Default: 40000 (when not configured or set to 0).
	MaxOldToolCallTokens int `json:"max_old_tool_call_tokens,omitempty"`

	// Starred indicates if this session has been starred by the user
	Starred bool `json:"starred"`

	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	Cost         float64 `json:"cost"`

	// Permissions holds session-level permission overrides.
	// When set, these are evaluated before team-level permissions.
	Permissions *PermissionsConfig `json:"permissions,omitempty"`

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

	// AgentName, when set, tells RunStream which agent to use for this session
	// instead of reading from the shared runtime currentAgent field. This is
	// required for background agent tasks where multiple sessions may run
	// concurrently on different agents.
	AgentName string `json:"-"`

	// ParentID indicates this is a sub-session created by task transfer.
	// Sub-sessions are not persisted as standalone entries; they are embedded
	// within the parent session's Messages array.
	ParentID string `json:"-"`

	// MessageUsageHistory stores per-message usage data for remote mode.
	// In remote mode, messages are managed server-side, so we track usage separately.
	// This is not persisted (json:"-") as it's only needed for the current session display.
	MessageUsageHistory []MessageUsageRecord `json:"-"`
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

// Message is a message from an agent
type Message struct {
	// ID is the database ID of the message (used for persistence tracking)
	ID        int64        `json:"-"`
	AgentName string       `json:"agentName"` // TODO: rename to agent_name
	Message   chat.Message `json:"message"`
	// Implicit is an optional field to indicate if the message shouldn't be shown to the user. It's needed for special  situations
	// like when an agent transfers a task to another agent - new session is created with a default user message, but this shouldn't be shown to the user.
	// Such messages should be marked as true
	Implicit bool `json:"implicit,omitempty"`
}

func ImplicitUserMessage(content string) *Message {
	msg := UserMessage(content)
	msg.Implicit = true
	return msg
}

func UserMessage(content string, multiContent ...chat.MessagePart) *Message {
	return &Message{
		Message: chat.Message{
			Role:         chat.MessageRoleUser,
			Content:      content,
			MultiContent: multiContent,
			CreatedAt:    nowFn().Format(time.RFC3339),
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
	return &Message{
		Message: chat.Message{
			Role:      chat.MessageRoleSystem,
			Content:   content,
			CreatedAt: nowFn().Format(time.RFC3339),
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

// EvalResult contains the evaluation scoring outcome for a session.
type EvalResult struct {
	Passed       bool             `json:"passed"`
	Successes    []string         `json:"successes,omitempty"`
	Failures     []string         `json:"failures,omitempty"`
	Error        string           `json:"error,omitempty"`
	Cost         float64          `json:"cost"`
	OutputTokens int64            `json:"output_tokens"`
	Checks       EvalResultChecks `json:"checks"`
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
// cannot mutate the snapshot; non-Message fields (Summary, SubSession, Cost,
// FirstKeptEntry) are shallow-copied since they are not mutated in place.
func (s *Session) snapshotItems() []Item {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]Item, len(s.Messages))
	for i, item := range s.Messages {
		items[i] = item
		if item.Message != nil {
			items[i].Message = cloneMessage(item.Message)
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
		m.ToolDefinitions = slices.Clone(m.ToolDefinitions)
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

// Session helper methods

// AddMessage adds a message to the session
func (s *Session) AddMessage(msg *Message) {
	s.mu.Lock()
	s.Messages = append(s.Messages, NewMessageItem(msg))
	s.mu.Unlock()
}

// SetUsage records cumulative input/output token counts under s.mu.
// The runtime stream goroutine and the persistence observer race on
// these fields without it.
func (s *Session) SetUsage(input, output int64) {
	s.mu.Lock()
	s.InputTokens = input
	s.OutputTokens = output
	s.mu.Unlock()
}

// Usage returns a consistent snapshot of the cumulative input/output
// token counts.
func (s *Session) Usage() (input, output int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.InputTokens, s.OutputTokens
}

// ApplyCompaction atomically resets the session's cumulative token
// counts and appends a summary item under s.mu so concurrent readers
// (e.g. the persistence observer's UpdateSession snapshot) cannot
// observe the new tokens without the matching summary item.
func (s *Session) ApplyCompaction(inputTokens, outputTokens int64, item Item) {
	s.mu.Lock()
	s.InputTokens = inputTokens
	s.OutputTokens = outputTokens
	s.Messages = append(s.Messages, item)
	s.mu.Unlock()
}

// AddSubSession adds a sub-session to the session
func (s *Session) AddSubSession(subSession *Session) {
	s.mu.Lock()
	s.Messages = append(s.Messages, NewSubSessionItem(subSession))
	s.mu.Unlock()
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

// AttachedFilesSnapshot returns a copy of the session's attached file paths.
// Callers may freely mutate the returned slice without affecting the session.
func (s *Session) AttachedFilesSnapshot() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.AttachedFiles)
}

type Opt func(s *Session)

func WithUserMessage(content string) Opt {
	return func(s *Session) {
		s.AddMessage(UserMessage(content))
	}
}

func WithImplicitUserMessage(content string) Opt {
	return func(s *Session) {
		s.AddMessage(ImplicitUserMessage(content))
	}
}

func WithSystemMessage(content string) Opt {
	return func(s *Session) {
		s.AddMessage(SystemMessage(content))
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
// Set to -1 to disable truncation (unlimited tool content).
// Set to 0 to use the default (40000).
func WithMaxOldToolCallTokens(n int) Opt {
	return func(s *Session) {
		s.MaxOldToolCallTokens = n
	}
}

func WithWorkingDir(workingDir string) Opt {
	return func(s *Session) {
		s.WorkingDir = workingDir
	}
}

func WithTitle(title string) Opt {
	return func(s *Session) {
		s.Title = title
	}
}

func WithMessages(messages []Item) Opt {
	return func(s *Session) {
		s.Messages = messages
	}
}

func WithToolsApproved(toolsApproved bool) Opt {
	return func(s *Session) {
		s.ToolsApproved = toolsApproved
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
		s.Permissions = perms
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

// WithID sets the session ID. If not set, a UUID will be generated.
func WithID(id string) Opt {
	return func(s *Session) {
		s.ID = id
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

// New creates a new agent session
func New(opts ...Opt) *Session {
	s := &Session{
		ID:              newIDFn(),
		CreatedAt:       nowFn(),
		SendUserMessage: true,
	}

	for _, opt := range opts {
		opt(s)
	}

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

// buildSessionSummaryMessages builds system messages containing the session summary
// if one exists. Session summaries are context-specific per session and thus should not have a checkpoint (they will be cached alongside the first user message anyway)
//
// startIndex is the index in items from which conversation messages should be
// emitted. When a summary with FirstKeptEntry is present, this points to the
// first kept message so that recent context is preserved after compaction.
// Otherwise it is lastSummaryIndex+1 (i.e. right after the summary item), or
// 0 when there is no summary.
func buildSessionSummaryMessages(items []Item) ([]chat.Message, int) {
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

	if lastSummaryIndex >= 0 && lastSummaryIndex < len(items) {
		messages = append(messages, chat.Message{
			Role:      chat.MessageRoleUser,
			Content:   "Session Summary: " + items[lastSummaryIndex].Summary,
			CreatedAt: nowFn().Format(time.RFC3339),
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

	return messages, startIndex
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
// All work is performed under s.mu.RLock via snapshotItems, so this
// method is safe to call concurrently with AddMessage / ApplyCompaction
// on the same session.
func (s *Session) CompactionInput() ([]chat.Message, []int) {
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
			Content:   "Session Summary: " + items[lastSummaryIndex].Summary,
			CreatedAt: nowFn().Format(time.RFC3339),
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
		messages = append(messages, msg)
		sessIndices = append(sessIndices, i)
	}
	return messages, sessIndices
}

func (s *Session) GetMessages(a *agent.Agent, extraSystemMessages ...chat.Message) []chat.Message {
	slog.Debug("Getting messages for agent", "agent", a.Name(), "session_id", s.ID)

	// Build invariant system messages (cacheable across sessions/users/projects)
	invariantMessages := buildInvariantSystemMessages(a)
	markLastMessageAsCacheControl(invariantMessages)

	// Take a snapshot of Messages under the lock, copying Message structs
	// to avoid racing with UpdateMessage which may modify the pointed-to objects.
	items := s.snapshotItems()

	// Build session summary messages (vary per session)
	summaryMessages, startIndex := buildSessionSummaryMessages(items)

	var messages []chat.Message
	messages = append(messages, invariantMessages...)
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

	// Begin adding conversation messages
	for i := startIndex; i < len(items); i++ {
		item := items[i]
		if item.IsMessage() {
			messages = append(messages, item.Message.Message)
		}
	}

	maxItems := a.NumHistoryItems()
	if maxItems > 0 {
		messages = trimMessages(messages, maxItems)
	}

	// Use configured max tokens or fall back to default constant if zero or unset.
	// -1 means unlimited (no truncation).
	maxOldToolCallTokens := s.MaxOldToolCallTokens
	if maxOldToolCallTokens == 0 {
		maxOldToolCallTokens = DefaultMaxOldToolCallTokens
	}
	if maxOldToolCallTokens > 0 { // If maxOldToolCallTokens is -1, skip truncation (unlimited)
		messages = truncateOldToolContent(messages, maxOldToolCallTokens)
	}

	messages = normalizeMessageContent(messages)
	messages = sanitizeToolCalls(messages)

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

	return messages
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

// sanitizeToolCalls ensures every tool call in assistant messages has a
// corresponding tool-result message. It walks the message list tracking
// pending tool calls; when a tool-result message arrives its ID is marked
// fulfilled. When the next assistant or user message is encountered (or the
// end of the list is reached), any still-pending tool calls receive synthetic
// error results injected just before that boundary. This guarantees the
// provider always sees a valid request/response pair for every tool call.
func sanitizeToolCalls(messages []chat.Message) []chat.Message {
	var (
		out              []chat.Message
		pendingToolCalls []tools.ToolCall
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
		resultIDs = make(map[string]bool)
	}

	for _, msg := range messages {
		switch {
		case msg.Role == chat.MessageRoleTool:
			if msg.ToolCallID != "" {
				resultIDs[msg.ToolCallID] = true
			}

		case msg.Role == chat.MessageRoleAssistant && len(msg.ToolCalls) > 0:
			flushPending()
			out = append(out, msg)
			pendingToolCalls = msg.ToolCalls
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
