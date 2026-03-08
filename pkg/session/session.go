package session

import (
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/docker/cagent/pkg/agent"
	"github.com/docker/cagent/pkg/chat"
	"github.com/docker/cagent/pkg/tools"
)

// Item represents either a message or a sub-session
type Item struct {
	// Message holds a regular conversation message
	Message *Message `json:"message,omitempty"`

	// SubSession holds a complete sub-session from task transfers
	SubSession *Session `json:"sub_session,omitempty"`

	// Summary is a summary of the session up until this point
	Summary string `json:"summary,omitempty"`

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

	// Messages holds the conversation history (messages and sub-sessions)
	Messages []Item `json:"messages"`

	// CreatedAt is the time the session was created
	CreatedAt time.Time `json:"created_at"`

	// ToolsApproved is a flag to indicate if the tools have been approved
	ToolsApproved bool `json:"tools_approved"`

	// Thinking is a session-level flag to enable thinking/interleaved thinking
	// defaults for all providers. When false, providers will not apply auto-thinking budgets
	// or interleaved thinking, regardless of model config. This is controlled by the /think
	// command in the TUI. Defaults to true (thinking enabled).
	Thinking bool `json:"thinking"`

	// HideToolResults is a flag to indicate if tool results should be hidden
	HideToolResults bool `json:"hide_tool_results"`

	// WorkingDir is the base directory used for filesystem-aware tools
	WorkingDir string `json:"working_dir,omitempty"`

	// SendUserMessage is a flag to indicate if the user message should be sent
	SendUserMessage bool

	// MaxIterations is the maximum number of agentic loop iterations to prevent infinite loops
	// If 0, there is no limit
	MaxIterations int `json:"max_iterations"`

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

	// BranchParentSessionID indicates this session was branched from another session.
	BranchParentSessionID string `json:"branch_parent_session_id,omitempty"`

	// BranchParentPosition is the parent session item position where this branch occurred.
	// Only set when BranchParentSessionID is non-empty.
	BranchParentPosition *int `json:"branch_parent_position,omitempty"`

	// BranchCreatedAt is the time when this branch session was created.
	BranchCreatedAt *time.Time `json:"branch_created_at,omitempty"`

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
			CreatedAt:    time.Now().Format(time.RFC3339),
		},
	}
}

func NewAgentMessage(a *agent.Agent, message *chat.Message) *Message {
	return &Message{
		AgentName: a.Name(),
		Message:   *message,
	}
}

func SystemMessage(content string) *Message {
	return &Message{
		Message: chat.Message{
			Role:      chat.MessageRoleSystem,
			Content:   content,
			CreatedAt: time.Now().Format(time.RFC3339),
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

// EvalCriteria contains the evaluation criteria for a session.
type EvalCriteria struct {
	Relevance  []string `json:"relevance"`             // Statements that should be true about the response
	WorkingDir string   `json:"working_dir,omitempty"` // Subdirectory under evals/working_dirs/
	Size       string   `json:"size,omitempty"`        // Expected response size: S, M, L, XL
	Setup      string   `json:"setup,omitempty"`       // Optional sh script to run in the container before docker agent run --exec
}

// deepCopyMessage returns a deep copy of a session Message.
// It copies the inner chat.Message's slice and pointer fields so that the
// returned value shares no mutable state with the original.
func deepCopyMessage(m *Message) *Message {
	cp := *m
	cp.Message = deepCopyChatMessage(m.Message)
	return &cp
}

// deepCopyChatMessage returns a deep copy of a chat.Message, duplicating
// all slice and pointer fields that would otherwise alias the original.
func deepCopyChatMessage(m chat.Message) chat.Message {
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
		m.ToolCalls = append([]tools.ToolCall(nil), m.ToolCalls...)
	}
	if m.ToolDefinitions != nil {
		m.ToolDefinitions = append([]tools.Tool(nil), m.ToolDefinitions...)
	}
	if m.Usage != nil {
		usageCopy := *m.Usage
		m.Usage = &usageCopy
	}
	if m.ThoughtSignature != nil {
		m.ThoughtSignature = append([]byte(nil), m.ThoughtSignature...)
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
	s.mu.RLock()
	items := make([]Item, len(s.Messages))
	for i, item := range s.Messages {
		if item.Message != nil {
			items[i] = Item{Message: deepCopyMessage(item.Message)}
		} else {
			items[i] = item
		}
	}
	s.mu.RUnlock()

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
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Message.Role == role {
			return strings.TrimSpace(messages[i].Message.Content)
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
	s.MessageUsageHistory = append(s.MessageUsageHistory, MessageUsageRecord{
		AgentName: agentName,
		Model:     model,
		Cost:      cost,
		Usage:     *usage,
	})
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

func WithToolsApproved(toolsApproved bool) Opt {
	return func(s *Session) {
		s.ToolsApproved = toolsApproved
	}
}

func WithThinking(thinking bool) Opt {
	return func(s *Session) {
		s.Thinking = thinking
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
	sessionID := uuid.New().String()
	slog.Debug("Creating new session", "session_id", sessionID)

	s := &Session{
		ID:              sessionID,
		CreatedAt:       time.Now(),
		SendUserMessage: true,
		Thinking:        false,
	}

	for _, opt := range opts {
		opt(s)
	}

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
			Content: "You are a multi-agent system, make sure to answer the user query in the most helpful way possible. You have access to these sub-agents:\n" + text.String() + "\nIMPORTANT: You can ONLY transfer tasks to the agents listed above using their ID. The valid agent names are: " + strings.Join(validAgentIDs, ", ") + ". You MUST NOT attempt to transfer to any other agent IDs - doing so will cause system errors.\n\nIf you are the best to answer the question according to your description, you can answer it.\n\nIf another agent is better for answering the question according to its description, call `transfer_task` function to transfer the question to that agent using the agent's ID. When transferring, do not generate any text other than the function call.\n\n",
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

// buildContextSpecificSystemMessages builds system messages that vary
// per user, project, or time. These messages should come after
// the invariant checkpoint to maintain optimal caching behavior.
//
// These messages depend on runtime context (working directory, current date,
// user-specific skills) and cannot be cached across sessions or users.
// Note: Session summary is handled separately in buildSessionSummaryMessages.
func buildContextSpecificSystemMessages(a *agent.Agent, s *Session) []chat.Message {
	var messages []chat.Message

	if a.AddDate() {
		messages = append(messages, chat.Message{
			Role:    chat.MessageRoleSystem,
			Content: "Today's date: " + time.Now().Format("2006-01-02"),
		})
	}

	wd := s.WorkingDir
	if wd == "" {
		var err error
		wd, err = os.Getwd()
		if err != nil {
			slog.Error("getting current working directory for environment info", "error", err)
		}
	}
	if wd != "" {
		if a.AddEnvironmentInfo() {
			messages = append(messages, chat.Message{
				Role:    chat.MessageRoleSystem,
				Content: getEnvironmentInfo(wd),
			})
		}

		for _, prompt := range a.AddPromptFiles() {
			additionalPrompts, err := readPromptFiles(wd, prompt)
			if err != nil {
				slog.Error("reading prompt file", "file", prompt, "error", err)
				continue
			}

			for _, additionalPrompt := range additionalPrompts {
				messages = append(messages, chat.Message{
					Role:    chat.MessageRoleSystem,
					Content: additionalPrompt,
				})
			}
		}
	}

	return messages
}

// buildSessionSummaryMessages builds system messages containing the session summary
// if one exists. Session summaries are context-specific per session and thus should not have a checkpoint (they will be cached alongside the first user message anyway)
//
// lastSummaryIndex is the index of the last summary item in s.Messages, or -1 if none exists.
func buildSessionSummaryMessages(items []Item) ([]chat.Message, int) {
	var messages []chat.Message
	// Find the last summary index to determine where conversation messages start
	// and to include the summary in session summary messages
	lastSummaryIndex := -1
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Summary != "" {
			lastSummaryIndex = i
			break
		}
	}

	if lastSummaryIndex >= 0 && lastSummaryIndex < len(items) {
		messages = append(messages, chat.Message{
			Role:      chat.MessageRoleUser,
			Content:   "Session Summary: " + items[lastSummaryIndex].Summary,
			CreatedAt: time.Now().Format(time.RFC3339),
		})
	}

	return messages, lastSummaryIndex
}

func (s *Session) GetMessages(a *agent.Agent) []chat.Message {
	slog.Debug("Getting messages for agent", "agent", a.Name(), "session_id", s.ID)

	// Build invariant system messages (cacheable across sessions/users/projects)
	invariantMessages := buildInvariantSystemMessages(a)
	markLastMessageAsCacheControl(invariantMessages)

	// Build context-specific system messages (vary per user/project/time)
	contextMessages := buildContextSpecificSystemMessages(a, s)
	markLastMessageAsCacheControl(contextMessages)

	// Take a snapshot of Messages under the lock, copying Message structs
	// to avoid racing with UpdateMessage which may modify the pointed-to objects.
	s.mu.RLock()
	items := make([]Item, len(s.Messages))
	for i, item := range s.Messages {
		if item.Message != nil {
			items[i] = Item{Message: deepCopyMessage(item.Message), Summary: item.Summary, SubSession: item.SubSession, Cost: item.Cost}
		} else {
			items[i] = item
		}
	}
	s.mu.RUnlock()

	// Build session summary messages (vary per session)
	summaryMessages, lastSummaryIndex := buildSessionSummaryMessages(items)

	var messages []chat.Message
	messages = append(messages, invariantMessages...)
	messages = append(messages, contextMessages...)
	messages = append(messages, summaryMessages...)

	startIndex := lastSummaryIndex + 1

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
