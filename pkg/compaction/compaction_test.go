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
			msg:      chat.Message{Content: "Hello, world!"}, // 13 chars → 13/3.5 = 3 + 5 = 8
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
			// 21 total chars → 21/3.5 = 6 + 5 overhead = 11
			expected: 11,
		},
		{
			// Regression test for the runtime-generated assistant shape
			// (pkg/runtime.recordAssistantMessage /
			// stripGeneratedMediaTransform) that mirrors Content into a
			// MultiContent text part verbatim, so oaistream-style converters
			// treating MultiContent as authoritative don't lose the text.
			// The mirrored part must be counted once, not twice.
			name: "content mirrored into a multi-content text part is not double-counted",
			msg: chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "here you go", // 11 chars
				MultiContent: []chat.MessagePart{
					{Type: chat.MessagePartTypeText, Text: "here you go"}, // mirror of Content, must be skipped
					{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
						Name: "cat.png", MimeType: "image/png",
						Source: chat.DocumentSource{ArtifactPath: "cat.png"},
					}},
				},
			},
			// 11 chars (Content, counted once) → 11/3.5 = 3 + 5 overhead = 8.
			// ArtifactPath-referenced generated media carries no InlineData, so
			// it draws no binary-attachment charge here; what this case checks
			// is that the mirrored text part does NOT add another 3 on top
			// (which would make it 11).
			expected: 8,
		},
		{
			// A MultiContent text part that happens to repeat Content's exact
			// text but is NOT the mirror (it comes after another part with
			// the same text already skipped) must still be counted: only the
			// first match is treated as the mirror, so genuinely repeated
			// user-authored text is never silently dropped from the estimate.
			name: "only the first multi-content match of Content is treated as the mirror",
			msg: chat.Message{
				Content: "same", // 4 chars
				MultiContent: []chat.MessagePart{
					{Type: chat.MessagePartTypeText, Text: "same"}, // skipped as the mirror
					{Type: chat.MessagePartTypeText, Text: "same"}, // counted: 4 chars
				},
			},
			// 4 (Content) + 4 (second "same") = 8 chars → 8/3.5 = 2 + 5 overhead = 7
			expected: 7,
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
			// 33 chars → 33/3.5 = 9 + 5 overhead = 14
			expected: 14,
		},
		{
			name: "message with reasoning content",
			msg: chat.Message{
				Content:          "answer",                                         // 6 chars
				ReasoningContent: "Let me think about this carefully step by step", // 46 chars
			},
			// 52 total chars → 52/3.5 = 14 + 5 overhead = 19
			expected: 19,
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
			// 38 total chars → 38/3.5 = 10 + 5 overhead = 15
			expected: 15,
		},
		{
			name: "inline document text is counted",
			msg: chat.Message{
				MultiContent: []chat.MessagePart{
					{
						Type: chat.MessagePartTypeDocument,
						Document: &chat.Document{
							Name:     "notes.txt",
							MimeType: "text/plain",
							Source:   chat.DocumentSource{InlineText: strings.Repeat("x", 35)},
						},
					},
				},
			},
			// 35 chars → 35/3.5 = 10 + 5 overhead = 15
			expected: 15,
		},
		{
			name: "binary attachments get a flat charge",
			msg: chat.Message{
				MultiContent: []chat.MessagePart{
					{
						Type: chat.MessagePartTypeDocument,
						Document: &chat.Document{
							Name:     "screenshot.png",
							MimeType: "image/png",
							Source:   chat.DocumentSource{InlineData: []byte{0x89, 0x50}},
						},
					},
					{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "data:image/png;base64,xyz"}},
				},
			},
			// 2 binary parts × 1500 + 5 overhead; base64 payloads are
			// billed as image tokens by providers, never as text.
			expected: 3_005,
		},
		{
			name: "provider-reported usage wins over the heuristic",
			msg: chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: strings.Repeat("a", 4_000), // heuristic would say ~1147
				Usage:   &chat.Usage{InputTokens: 50_000, OutputTokens: 900},
			},
			// 900 reported + 5 overhead; InputTokens describe the prompt,
			// not this message, and must not leak into the estimate.
			expected: 905,
		},
		{
			name: "reasoning tokens are excluded when reasoning is not stored",
			msg: chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "short answer",
				Usage:   &chat.Usage{OutputTokens: 5_000, ReasoningTokens: 4_990},
			},
			// o-series style: 4990 hidden reasoning tokens never come back
			// as input → only 10 visible tokens + 5 overhead.
			expected: 15,
		},
		{
			name: "reasoning tokens stay counted when reasoning is stored",
			msg: chat.Message{
				Role:             chat.MessageRoleAssistant,
				Content:          "answer",
				ReasoningContent: "stored thinking block",
				Usage:            &chat.Usage{OutputTokens: 1_000, ReasoningTokens: 800},
			},
			// Anthropic style: the thinking block is resent as input, so
			// the full output count applies.
			expected: 1_005,
		},
		{
			name: "usage without output tokens falls back to the heuristic",
			msg: chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "Hello, world!", // 13 chars → 3 + 5 = 8
				Usage:   &chat.Usage{InputTokens: 1_000},
			},
			expected: 8,
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

// usageMsg builds an assistant anchor message: a turn whose usage
// carries the provider-reported prompt and output token counts.
func usageMsg(model string, promptTokens, outputTokens int64) chat.Message {
	return chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "ok",
		Model:   model,
		Usage:   &chat.Usage{InputTokens: promptTokens, OutputTokens: outputTokens},
	}
}

func TestNewEstimator(t *testing.T) {
	t.Parallel()

	// 7000-char tool result → heuristic 7000/3.5 + 5 = 2005 tokens.
	toolResult := chat.Message{Role: chat.MessageRoleTool, Content: strings.Repeat("x", 7_000)}

	t.Run("no usage data yields a neutral estimator", func(t *testing.T) {
		t.Parallel()
		e := NewSliceEstimator([]chat.Message{
			{Role: chat.MessageRoleUser, Content: strings.Repeat("a", 10_000)},
			{Role: chat.MessageRoleAssistant, Content: "reply"},
		})
		assert.InEpsilon(t, 1.0, e.Scale(), 1e-9)
	})

	t.Run("underestimated content scales the heuristic up", func(t *testing.T) {
		t.Parallel()
		// Anchor 1 ends at total 1100; anchor 2 reports a prompt of
		// 4108, so the tool result in between cost 3008 real tokens
		// against a 2005 heuristic → ratio ≈ 1.5.
		e := NewSliceEstimator([]chat.Message{
			{Role: chat.MessageRoleUser, Content: "question"},
			usageMsg("test/m", 1_000, 100),
			toolResult,
			usageMsg("test/m", 4_108, 50),
		})
		assert.InDelta(t, 1.5, e.Scale(), 0.01)

		// The scale applies to heuristic estimates only.
		fresh := chat.Message{Role: chat.MessageRoleTool, Content: strings.Repeat("y", 3_500)}
		assert.InDelta(t, 1005*1.5, float64(e.EstimateMessageTokens(&fresh)), 2)

		reported := usageMsg("test/m", 9_999, 200)
		assert.Equal(t, int64(205), e.EstimateMessageTokens(&reported),
			"provider-reported counts must never be scaled")
	})

	t.Run("overestimated content scales down but never below the floor", func(t *testing.T) {
		t.Parallel()
		// Real cost 600 vs 2005 heuristic → raw ratio ≈0.3, clamped to 0.75.
		e := NewSliceEstimator([]chat.Message{
			usageMsg("test/m", 1_000, 100),
			toolResult,
			usageMsg("test/m", 1_700, 50),
		})
		assert.InEpsilon(t, 0.75, e.Scale(), 1e-9)
	})

	t.Run("inflated deltas are capped", func(t *testing.T) {
		t.Parallel()
		// Real cost 10000 vs 2005 heuristic → raw ratio ≈5, clamped to 2.
		e := NewSliceEstimator([]chat.Message{
			usageMsg("test/m", 1_000, 100),
			toolResult,
			usageMsg("test/m", 11_100, 50),
		})
		assert.InEpsilon(t, 2.0, e.Scale(), 1e-9)
	})

	t.Run("small samples are not trusted", func(t *testing.T) {
		t.Parallel()
		// 100 chars → ~33 heuristic tokens, below calibrationMinTokens.
		e := NewSliceEstimator([]chat.Message{
			usageMsg("test/m", 1_000, 100),
			{Role: chat.MessageRoleTool, Content: strings.Repeat("x", 100)},
			usageMsg("test/m", 11_100, 50),
		})
		assert.InEpsilon(t, 1.0, e.Scale(), 1e-9)
	})

	t.Run("negative deltas from a mid-window compaction are discarded", func(t *testing.T) {
		t.Parallel()
		// The second window's prompt shrank (compaction rebuilt it), so
		// only the first window's clean 1.5 ratio survives.
		e := NewSliceEstimator([]chat.Message{
			usageMsg("test/m", 1_000, 100),
			toolResult,
			usageMsg("test/m", 4_108, 50),
			toolResult,
			usageMsg("test/m", 500, 50),
		})
		assert.InDelta(t, 1.5, e.Scale(), 0.01)
	})

	t.Run("windows spanning a model switch are discarded", func(t *testing.T) {
		t.Parallel()
		e := NewSliceEstimator([]chat.Message{
			usageMsg("openai/gpt-5", 1_000, 100),
			toolResult,
			usageMsg("anthropic/claude", 11_100, 50),
		})
		assert.InEpsilon(t, 1.0, e.Scale(), 1e-9)
	})

	t.Run("exactly sized in-between messages are subtracted from the delta", func(t *testing.T) {
		t.Parallel()
		// The in-between assistant turn has an output-only usage report
		// (5000 tokens, no prompt data → not an anchor). Its exact size
		// must not dilute the tool result's 2005-vs-3008 ratio.
		interAssistant := chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "large reply",
			Model:   "test/m",
			Usage:   &chat.Usage{OutputTokens: 5_000},
		}
		e := NewSliceEstimator([]chat.Message{
			usageMsg("test/m", 1_000, 100),
			interAssistant,
			toolResult,
			usageMsg("test/m", 9_113, 50), // delta 8013 − (5000+5) reported = 3008
		})
		assert.InDelta(t, 1.5, e.Scale(), 0.01)
	})
}

// TestSplitIndexForKeep_UsageCalibration verifies that provider-reported
// usage recorded on assistant turns shifts the keep boundary: content
// the heuristic undercounts 2× fills the keep budget twice as fast.
func TestSplitIndexForKeep_UsageCalibration(t *testing.T) {
	t.Parallel()

	// Each user message: 40000 chars → heuristic 11433 tokens, but the
	// anchor deltas report 22866 real tokens (ratio exactly 2.0).
	messages := []chat.Message{
		{Role: chat.MessageRoleUser, Content: strings.Repeat("u", 40_000)},
		usageMsg("test/m", 15_000, 100),
		{Role: chat.MessageRoleUser, Content: strings.Repeat("v", 40_000)},
		usageMsg("test/m", 37_966, 100), // 15100 + 22866
	}

	// Uncalibrated walk would fit messages[1:] in 23k (105 + 11433 +
	// 105); with the observed 2× correction the kept tail shrinks to
	// messages[2:] (105 + 22866).
	assert.Equal(t, 2, SplitIndexForKeep(messages, 23_000))
}

func TestShouldCompact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		input        int64
		output       int64
		added        int64
		contextLimit int64
		threshold    float64
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
		{
			name:         "custom threshold triggers earlier than default",
			input:        60000,
			output:       0,
			added:        0,
			contextLimit: 100000,
			threshold:    0.5,
			want:         true, // 60000 > 50000, would not trigger at the 0.9 default
		},
		{
			name:         "custom threshold not reached",
			input:        40000,
			output:       0,
			added:        0,
			contextLimit: 100000,
			threshold:    0.5,
			want:         false,
		},
		{
			name:         "threshold of 1 only triggers past the full window",
			input:        99999,
			output:       0,
			added:        0,
			contextLimit: 100000,
			threshold:    1,
			want:         false,
		},
		{
			name:         "zero threshold falls back to the 90% default",
			input:        90001,
			output:       0,
			added:        0,
			contextLimit: 100000,
			threshold:    0,
			want:         true,
		},
		{
			name:         "negative threshold falls back to the 90% default",
			input:        89000,
			output:       0,
			added:        0,
			contextLimit: 100000,
			threshold:    -0.5,
			want:         false,
		},
		{
			name:         "threshold above 1 falls back to the 90% default",
			input:        90001,
			output:       0,
			added:        0,
			contextLimit: 100000,
			threshold:    1.5,
			want:         true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ShouldCompact(tt.input, tt.output, tt.added, tt.contextLimit, tt.threshold)
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
				msg(chat.MessageRoleUser, strings.Repeat("a", 40000)),      // ~11433 tokens
				msg(chat.MessageRoleAssistant, strings.Repeat("b", 40000)), // ~11433 tokens
				msg(chat.MessageRoleUser, strings.Repeat("c", 40000)),      // ~11433 tokens
				msg(chat.MessageRoleAssistant, strings.Repeat("d", 40000)), // ~11433 tokens
				msg(chat.MessageRoleUser, strings.Repeat("e", 40000)),      // ~11433 tokens
				msg(chat.MessageRoleAssistant, strings.Repeat("f", 40000)), // ~11433 tokens
			},
			maxTokens: 23_000, // enough for exactly 2 messages
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
			budget:    2400, // ~2 messages worth (~1147 tokens each)
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
