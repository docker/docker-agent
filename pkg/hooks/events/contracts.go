// Package events defines the shared contracts for hook configuration and execution.
package events

import (
	"iter"
	"slices"
)

// Rewrite identifies the payload a sequential hook pipeline can replace.
type Rewrite uint8

const (
	RewriteNone Rewrite = iota
	RewriteToolInput
	RewriteMessages
	RewriteToolResponse
)

// Contract describes an event's configuration shape and runtime capabilities.
type Contract struct {
	Name               string
	ToolMatched        bool
	CanBlock           bool
	FailClosed         bool
	Rewrite            Rewrite
	Decision           bool
	PermissionApproval bool
	Metadata           bool
	Context            bool
	Instructions       bool
	Summary            bool
}

// Sequential reports whether each hook must receive the preceding rewrite.
func (c Contract) Sequential() bool { return c.Rewrite != RewriteNone }

// Permission reports whether permission_decision is meaningful on this event.
func (c Contract) Permission() bool { return c.Decision || c.PermissionApproval }

var contracts = []Contract{
	{Name: "pre_tool_use", ToolMatched: true, CanBlock: true, FailClosed: true, Rewrite: RewriteToolInput, Decision: true},
	{Name: "post_tool_use", ToolMatched: true, CanBlock: true},
	{Name: "permission_request", ToolMatched: true, CanBlock: true, PermissionApproval: true, Metadata: true},
	{Name: "session_start", Context: true, Instructions: true},
	{Name: "user_prompt_submit", CanBlock: true, Context: true},
	{Name: "user_steering_messages_submit", CanBlock: true, Context: true},
	{Name: "user_followup_submit", CanBlock: true, Context: true},
	{Name: "turn_start", Context: true, Instructions: true},
	{Name: "turn_end"},
	{Name: "before_llm_call", CanBlock: true, Rewrite: RewriteMessages},
	{Name: "after_llm_call"},
	{Name: "session_end"},
	{Name: "pre_compact", CanBlock: true, Context: true},
	{Name: "subagent_stop"},
	{Name: "on_user_input"},
	{Name: "stop"},
	{Name: "notification"},
	{Name: "on_error"},
	{Name: "on_max_iterations"},
	{Name: "on_agent_switch"},
	{Name: "on_session_resume"},
	{Name: "on_tool_approval_decision"},
	{Name: "before_compaction", CanBlock: true, Summary: true},
	{Name: "after_compaction"},
	{Name: "tool_response_transform", ToolMatched: true, Rewrite: RewriteToolResponse},
	{Name: "tool_input_transform", ToolMatched: true, CanBlock: true, Rewrite: RewriteToolInput},
	{Name: "tool_guard", ToolMatched: true, CanBlock: true, FailClosed: true, Decision: true, Metadata: true},
	{Name: "worktree_create", CanBlock: true, Context: true},
}

// All iterates over public event contracts in configuration order.
func All() iter.Seq[Contract] { return slices.Values(contracts) }

// Lookup returns a contract by its public event name.
func Lookup(name string) (Contract, bool) {
	for _, c := range contracts {
		if c.Name == name {
			return c, true
		}
	}
	return Contract{}, false
}
