package compaction

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestEstimateMessageTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		msg      chat.Message
		expected int64
	}{
		{
			name:     "empty message returns overhead only",
			msg:      chat.Message{},
			expected: 5, // perMessageOverhead
		},
		{
			name:     "text-only message",
			msg:      chat.Message{Content: "Hello, world!"}, // 13 chars → 13/4 = 3 + 5 = 8
			expected: 8,
		},
		{
			name: "multi-content text parts",
			msg: chat.Message{
				MultiContent: []chat.MessagePart{
					{Type: chat.MessagePartTypeText, Text: "first part"},  // 10 chars
					{Type: chat.MessagePartTypeText, Text: "second part"}, // 11 chars
				},
			},
			// 21 total chars → 21/4 = 5 + 5 overhead = 10
			expected: 10,
		},
		{
			name: "message with tool calls",
			msg: chat.Message{
				ToolCalls: []tools.ToolCall{
					{
						Function: tools.FunctionCall{
							Name:      "read_file",                // 9 chars
							Arguments: `{"path":"/tmp/test.txt"}`, // 24 chars
						},
					},
				},
			},
			// 33 chars → 33/4 = 8 + 5 overhead = 13
			expected: 13,
		},
		{
			name: "message with reasoning content",
			msg: chat.Message{
				Content:          "answer",                                         // 6 chars
				ReasoningContent: "Let me think about this carefully step by step", // 47 chars
			},
			// 53 chars → 53/4 = 13 + 5 overhead = 18
			expected: 18,
		},
		{
			name: "combined content types",
			msg: chat.Message{
				Content:          "result",                                   // 6 chars
				ReasoningContent: "thinking",                                 // 8 chars
				MultiContent:     []chat.MessagePart{{Text: "extra detail"}}, // 12 chars
				ToolCalls: []tools.ToolCall{
					{Function: tools.FunctionCall{Name: "cmd", Arguments: `{"x":"y"}`}}, // 3 + 9 = 12 chars
				},
			},
			// 38 total chars → 38/4 = 9 + 5 overhead = 14
			expected: 14,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := EstimateMessageTokens(&tt.msg)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestShouldCompact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		input        int64
		output       int64
		added        int64
		contextLimit int64
		want         bool
	}{
		{
			name:         "below threshold",
			input:        5000,
			output:       2000,
			added:        0,
			contextLimit: 100000,
			want:         false,
		},
		{
			name:         "exactly at 90% boundary",
			input:        90000,
			output:       0,
			added:        0,
			contextLimit: 100000,
			want:         false, // 90000 == int64(100000*0.9), need > not >=
		},
		{
			name:         "just above 90% threshold",
			input:        90001,
			output:       0,
			added:        0,
			contextLimit: 100000,
			want:         true,
		},
		{
			name:         "tool results push past threshold",
			input:        70000,
			output:       10000,
			added:        15000,
			contextLimit: 100000,
			want:         true, // 95000 > 90000
		},
		{
			name:         "zero context limit means unlimited",
			input:        999999,
			output:       999999,
			added:        999999,
			contextLimit: 0,
			want:         false,
		},
		{
			name:         "negative context limit means unlimited",
			input:        999999,
			output:       999999,
			added:        999999,
			contextLimit: -1,
			want:         false,
		},
		{
			name:         "all zeros",
			input:        0,
			output:       0,
			added:        0,
			contextLimit: 100000,
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ShouldCompact(tt.input, tt.output, tt.added, tt.contextLimit)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSplitIndexForKeep(t *testing.T) {
	t.Parallel()

	msg := func(role chat.MessageRole, content string) chat.Message {
		return chat.Message{Role: role, Content: content}
	}

	tests := []struct {
		name      string
		messages  []chat.Message
		maxTokens int64
		wantSplit int // expected split index
	}{
		{
			name:      "empty messages",
			messages:  nil,
			maxTokens: 1000,
			wantSplit: 0,
		},
		{
			name: "all messages fit in keep budget - returned len(messages) signals 'compact everything, keep nothing' (the manual /compact contract)",
			messages: []chat.Message{
				msg(chat.MessageRoleUser, "short"),
				msg(chat.MessageRoleAssistant, "short"),
			},
			maxTokens: 100_000,
			wantSplit: 2, // all fit → messages[:2] is everything to compact, messages[2:] is empty (nothing kept)
		},
		{
			name: "recent messages kept, older ones compacted",
			messages: []chat.Message{
				msg(chat.MessageRoleUser, strings.Repeat("a", 40000)),      // ~10005 tokens
				msg(chat.MessageRoleAssistant, strings.Repeat("b", 40000)), // ~10005 tokens
				msg(chat.MessageRoleUser, strings.Repeat("c", 40000)),      // ~10005 tokens
				msg(chat.MessageRoleAssistant, strings.Repeat("d", 40000)), // ~10005 tokens
				msg(chat.MessageRoleUser, strings.Repeat("e", 40000)),      // ~10005 tokens
				msg(chat.MessageRoleAssistant, strings.Repeat("f", 40000)), // ~10005 tokens
			},
			maxTokens: 20_100, // enough for exactly 2 messages
			wantSplit: 4,      // last 2 messages are kept
		},
		{
			name: "snap to assistant boundary even when a tool result fits",
			messages: []chat.Message{
				msg(chat.MessageRoleUser, "u1"),
				msg(chat.MessageRoleAssistant, "a1"),
				msg(chat.MessageRoleTool, "t1"),
			},
			maxTokens: 100_000, // everything fits
			wantSplit: 3,       // all fit → compact everything (returned len)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := SplitIndexForKeep(tt.messages, tt.maxTokens)
			assert.Equal(t, tt.wantSplit, got)
		})
	}
}

// TestSplitIndexForKeep_NeverReturnsZeroForNonEmptyInput pins the
// invariant that for any non-empty messages slice, SplitIndexForKeep
// returns a value in [1, len(messages)] — never 0.
//
// This matters because the compactor's firstKeptSessionIndex maps the
// returned split index back to a sess.Messages position, and when a
// prior summary exists, sessIndices[0] points at the prior summary
// item rather than at the start of the prior kept-tail. If 0 were
// reachable, the new FirstKeptEntry would land on the prior summary
// item and the next reconstruction would skip the prior kept-tail
// (the synthetic-summary user message inserted by
// session.buildSessionSummaryMessages would still appear, but the
// kept conversation between the prior FirstKeptEntry and the prior
// summary index would be lost).
//
// The implementation makes 0 unreachable: lastValidBoundary is
// initialised to len(messages); the only update path sets it to i
// (the current index), but that update happens AFTER the overflow
// check at iteration i, so a lastValidBoundary of 0 set at i=0 is
// never returned — the loop exits and the function falls through to
// `return len(messages)`. The overflow-path return therefore yields
// values ≥ 1, and the no-overflow path yields len(messages).
func TestSplitIndexForKeep_NeverReturnsZeroForNonEmptyInput(t *testing.T) {
	t.Parallel()

	msg := func(role chat.MessageRole, content string) chat.Message {
		return chat.Message{Role: role, Content: content}
	}

	cases := []struct {
		name      string
		messages  []chat.Message
		maxTokens int64
	}{
		{
			name:      "single user message that fits",
			messages:  []chat.Message{msg(chat.MessageRoleUser, "hi")},
			maxTokens: 1_000_000,
		},
		{
			name:      "single user message that overflows",
			messages:  []chat.Message{msg(chat.MessageRoleUser, strings.Repeat("x", 10_000))},
			maxTokens: 1,
		},
		{
			name: "first message is user/assistant and fits, rest overflows",
			messages: []chat.Message{
				msg(chat.MessageRoleUser, "u0"),
				msg(chat.MessageRoleAssistant, strings.Repeat("a", 40_000)),
				msg(chat.MessageRoleUser, strings.Repeat("b", 40_000)),
			},
			maxTokens: 5_000,
		},
		{
			name: "every message is user/assistant and everything fits (returns len)",
			messages: []chat.Message{
				msg(chat.MessageRoleUser, "u0"),
				msg(chat.MessageRoleAssistant, "a0"),
				msg(chat.MessageRoleUser, "u1"),
			},
			maxTokens: 1_000_000,
		},
		{
			name: "first message is the synthetic Session Summary user message (the prior-summary case)",
			messages: []chat.Message{
				msg(chat.MessageRoleUser, "Session Summary: "+strings.Repeat("s", 80_000)),
				msg(chat.MessageRoleUser, strings.Repeat("u", 40_000)),
				msg(chat.MessageRoleAssistant, strings.Repeat("a", 40_000)),
			},
			maxTokens: 5_000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SplitIndexForKeep(tc.messages, tc.maxTokens)
			assert.NotZero(t, got, "SplitIndexForKeep must not return 0 for non-empty input (would land FirstKeptEntry on the prior summary item and drop the prior kept-tail)")
			assert.GreaterOrEqual(t, got, 1)
			assert.LessOrEqual(t, got, len(tc.messages))
		})
	}
}

func TestFirstIndexInBudget(t *testing.T) {
	t.Parallel()

	msg := func(role chat.MessageRole, content string) chat.Message {
		return chat.Message{Role: role, Content: content}
	}

	tests := []struct {
		name      string
		messages  []chat.Message
		budget    int64
		wantFirst int
	}{
		{
			name:      "empty",
			messages:  nil,
			budget:    1000,
			wantFirst: 0,
		},
		{
			name: "everything fits",
			messages: []chat.Message{
				msg(chat.MessageRoleUser, "short"),
				msg(chat.MessageRoleAssistant, "short"),
			},
			budget:    1000,
			wantFirst: 0,
		},
		{
			name: "tight budget keeps tail starting on a user/assistant turn",
			messages: []chat.Message{
				msg(chat.MessageRoleUser, strings.Repeat("a", 4000)),
				msg(chat.MessageRoleAssistant, strings.Repeat("b", 4000)),
				msg(chat.MessageRoleUser, strings.Repeat("c", 4000)),
				msg(chat.MessageRoleAssistant, strings.Repeat("d", 4000)),
			},
			budget:    2100, // ~2 messages worth
			wantFirst: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := FirstIndexInBudget(tt.messages, tt.budget)
			assert.Equal(t, tt.wantFirst, got)
		})
	}
}
