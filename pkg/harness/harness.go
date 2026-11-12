// Package harness defines the contract between the runtime and external
// coding harnesses (Claude Code, Codex, ...) that agents configured with
// `harness:` delegate to. It carries no driver: pkg/codingharness implements
// it, and the runtime only links a driver once one is registered with
// runtime.RegisterHarness.
package harness

import (
	"context"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

// EventType identifies an Event.
type EventType string

const (
	EventText          EventType = "text"
	EventResult        EventType = "result"
	EventToolCallStart EventType = "tool_call_start"
	EventToolCallDelta EventType = "tool_call_delta"
	EventToolCall      EventType = "tool_call"
	EventToolResult    EventType = "tool_result"
	EventReasoning     EventType = "reasoning"
	EventSessionID     EventType = "session_id"
)

// Event is one streamed event from an external coding harness.
type Event struct {
	Type       EventType
	Text       string
	Result     string
	SessionID  string
	Usage      *Usage
	ToolID     string
	ToolName   string
	ToolArgs   string
	ToolOutput string
	ToolError  bool
	Reasoning  string
}

// Usage is the token and cost report a harness emits with its result.
type Usage struct {
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
	TotalCostUSD             float64
}

// Provider drives one external coding harness for an agent.
type Provider interface {
	Name() string
	Run(ctx context.Context, prompt string, handle func(Event)) error
	Resume(ctx context.Context, sessionID, prompt string, handle func(Event)) error
}

// Completer is an optional Provider capability: answering a one-shot prompt
// without tools or a persisted harness session, as session titles need.
type Completer interface {
	Provider
	Complete(ctx context.Context, systemPrompt, prompt string) (string, error)
}

// Eliciter is an optional Provider capability: putting the questions a
// harness asks mid-turn (Claude Code's AskUserQuestion and plan approval) to
// the user through elicit. Without it, a harness asks in plain text and ends
// its turn.
type Eliciter interface {
	Provider
	WithElicitation(elicit tools.ElicitationHandler) Provider
}

// Factory builds the Provider for a harness configuration.
type Factory func(cfg *latest.HarnessConfig) (Provider, error)
