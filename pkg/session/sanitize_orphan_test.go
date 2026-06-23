package session

import (
	"testing"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// assertToolPairingInvariant fails if any tool-result message lacks a matching
// tool_call ID in a preceding assistant message. This is exactly the invariant
// AWS Bedrock's ConverseStream enforces: "The number of toolResult blocks ...
// exceeds the number of toolUse blocks of previous turn." (issue #1676)
func assertToolPairingInvariant(t *testing.T, messages []chat.Message) {
	t.Helper()
	seen := map[string]bool{}
	for i, m := range messages {
		if m.Role == chat.MessageRoleAssistant {
			for _, tc := range m.ToolCalls {
				seen[tc.ID] = true
			}
		}
		if m.Role == chat.MessageRoleTool {
			if !seen[m.ToolCallID] {
				t.Errorf("orphaned tool result at index %d: tool_call_id %q has no preceding toolUse (Bedrock would reject this)",
					i, m.ToolCallID)
			}
		}
	}
}

// TestSanitizeToolCalls_DropsOrphanedToolResult is a regression test for issue
// #1676 (session resume fails on AWS Bedrock with a toolResult/toolUse count
// mismatch).
//
// Scenario: a session is compacted, and the kept-tail boundary (startIndex /
// FirstKeptEntry) lands *after* the assistant message that issued a tool call
// but *before* its tool result. On resume, buildSessionSummaryMessages emits
// the conversation from that boundary, so the reconstructed history begins with
// a tool-result message whose assistant (toolUse) was left behind the summary.
//
// sanitizeToolCalls is the final guard before the request hits the provider and
// must drop that orphaned result so the request stays valid.
func TestSanitizeToolCalls_DropsOrphanedToolResult(t *testing.T) {
	// History as reconstructed on resume after compaction: the toolUse-bearing
	// assistant message is gone (folded into the summary), only its result and
	// the following turn survive.
	reconstructed := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "Session Summary: ..."},
		// <-- assistant message with ToolCalls{ID:"tc1"} was dropped here
		{Role: chat.MessageRoleTool, ToolCallID: "tc1", Content: "file contents"},
		{Role: chat.MessageRoleAssistant, Content: "Here is the file."},
		{Role: chat.MessageRoleUser, Content: "thanks, now resume"},
	}

	out := sanitizeToolCalls(reconstructed)

	// The orphaned tool result must be gone, leaving a Bedrock-valid sequence.
	assertToolPairingInvariant(t, out)
	for i, m := range out {
		if m.Role == chat.MessageRoleTool && m.ToolCallID == "tc1" {
			t.Fatalf("orphaned tool result tc1 should have been dropped, still present at index %d", i)
		}
	}
}

// TestSanitizeToolCalls_KeepsMissingResultBalanced pins the opposite direction:
// a dangling toolUse (no result) gets a synthetic result and stays balanced.
func TestSanitizeToolCalls_KeepsMissingResultBalanced(t *testing.T) {
	messages := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "hi"},
		{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "tc1"}}},
		// result missing
		{Role: chat.MessageRoleUser, Content: "next"},
	}
	out := sanitizeToolCalls(messages)
	assertToolPairingInvariant(t, out) // synthetic result injected
}

// TestSanitizeToolCalls_DropsDuplicateToolResult guards the remaining
// toolResult/toolUse imbalance that orphan-dropping alone does not cover: a
// second tool_result carrying the same tool_call_id. Bedrock counts it as an
// extra toolResult block ("the number of toolResult blocks ... exceeds the
// number of toolUse blocks of previous turn"), the same ValidationException
// family as #1676 and #1593. Only the first result for a given tool_use should
// survive.
func TestSanitizeToolCalls_DropsDuplicateToolResult(t *testing.T) {
	messages := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "hi"},
		{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "tc1"}}},
		{Role: chat.MessageRoleTool, ToolCallID: "tc1", Content: "result"},
		{Role: chat.MessageRoleTool, ToolCallID: "tc1", Content: "duplicate result"},
		{Role: chat.MessageRoleUser, Content: "next"},
	}

	out := sanitizeToolCalls(messages)

	assertToolPairingInvariant(t, out)

	var count int
	for _, m := range out {
		if m.Role == chat.MessageRoleTool && m.ToolCallID == "tc1" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one tool result for tc1 after dedup, got %d", count)
	}
}

// TestGetMessages_ResumeAfterCompaction_NoOrphanedToolResult exercises the full
// GetMessages reconstruction path that runs on the first turn after a session
// is resumed (runtime/loop.go calls sess.GetMessages before the loop starts).
//
// It reproduces the exact #1676 condition: a compaction summary whose
// FirstKeptEntry boundary lands between an assistant tool_use and its
// tool_result, so buildSessionSummaryMessages emits the conversation starting at
// the orphaned tool_result. GetMessages must return a provider-valid sequence.
func TestGetMessages_ResumeAfterCompaction_NoOrphanedToolResult(t *testing.T) {
	testAgent := &agent.Agent{}
	s := New()

	// items[0]: old user turn (summarized away)
	s.AddMessage(NewAgentMessage("", &chat.Message{Role: chat.MessageRoleUser, Content: "old question"}))
	// items[1]: assistant that issued tool call tc1 (summarized away)
	s.AddMessage(NewAgentMessage("", &chat.Message{
		Role:      chat.MessageRoleAssistant,
		Content:   "calling tool",
		ToolCalls: []tools.ToolCall{{ID: "tc1"}},
	}))
	// items[2]: the tool result — KEPT, but its assistant (items[1]) is not
	s.AddMessage(NewAgentMessage("", &chat.Message{
		Role:       chat.MessageRoleTool,
		ToolCallID: "tc1",
		Content:    "tool output",
	}))
	// items[3]: assistant follow-up — kept
	s.AddMessage(NewAgentMessage("", &chat.Message{Role: chat.MessageRoleAssistant, Content: "answer"}))
	// items[4]: summary whose kept-tail boundary (index 2) splits the tc1 pair
	s.Messages = append(s.Messages, Item{Summary: "summary", FirstKeptEntry: 2})

	messages := s.GetMessages(testAgent)

	// The reconstructed history must not contain the orphaned tc1 result.
	assertToolPairingInvariant(t, messages)
}
