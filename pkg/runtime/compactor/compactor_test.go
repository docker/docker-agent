package compactor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

type fakeProvider struct{ id modelsdev.ID }

func (p fakeProvider) ID() modelsdev.ID { return p.id }

func (p fakeProvider) BaseConfig() base.Config { return base.Config{} }

func (p fakeProvider) CreateChatCompletionStream(
	context.Context,
	[]chat.Message,
	[]tools.Tool,
) (chat.MessageStream, error) {
	return nil, nil
}

func TestExtractMessages(t *testing.T) {
	t.Parallel()

	newMsg := func(role chat.MessageRole, content string) session.Item {
		return session.NewMessageItem(&session.Message{
			Message: chat.Message{Role: role, Content: content},
		})
	}

	tests := []struct {
		name                     string
		messages                 []session.Item
		contextLimit             int64
		additionalPrompt         string
		wantConversationMsgCount int
	}{
		{
			name:                     "empty session returns system and user prompt only",
			messages:                 nil,
			contextLimit:             100_000,
			wantConversationMsgCount: 0,
		},
		{
			name: "system messages are filtered out",
			messages: []session.Item{
				newMsg(chat.MessageRoleSystem, "system instruction"),
				newMsg(chat.MessageRoleUser, "hello"),
				newMsg(chat.MessageRoleAssistant, "hi"),
			},
			contextLimit:             100_000,
			wantConversationMsgCount: 2,
		},
		{
			name: "messages fit within context limit",
			messages: []session.Item{
				newMsg(chat.MessageRoleUser, "msg1"),
				newMsg(chat.MessageRoleAssistant, "msg2"),
				newMsg(chat.MessageRoleUser, "msg3"),
				newMsg(chat.MessageRoleAssistant, "msg4"),
			},
			contextLimit:             100_000,
			wantConversationMsgCount: 4,
		},
		{
			name: "truncation when context limit is very small",
			messages: []session.Item{
				newMsg(chat.MessageRoleUser, "first message with lots of content that takes tokens"),
				newMsg(chat.MessageRoleAssistant, "first response with lots of content that takes tokens"),
				newMsg(chat.MessageRoleUser, "second message"),
				newMsg(chat.MessageRoleAssistant, "second response"),
			},
			contextLimit:             MaxSummaryTokens + 50,
			wantConversationMsgCount: 0,
		},
		{
			name: "additional prompt is appended",
			messages: []session.Item{
				newMsg(chat.MessageRoleUser, "hello"),
			},
			contextLimit:             100_000,
			additionalPrompt:         "focus on code quality",
			wantConversationMsgCount: 1,
		},
		{
			name: "cost and cache control are cleared",
			messages: []session.Item{
				session.NewMessageItem(&session.Message{
					Message: chat.Message{
						Role:         chat.MessageRoleUser,
						Content:      "hello",
						Cost:         1.5,
						CacheControl: true,
					},
				}),
			},
			contextLimit:             100_000,
			wantConversationMsgCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sess := session.New(session.WithMessages(tt.messages))
			a := agent.New("test", "test prompt")
			result, _ := extractMessages(sess, a, tt.contextLimit, tt.additionalPrompt)

			require.GreaterOrEqual(t, len(result), tt.wantConversationMsgCount+2)
			assert.Equal(t, chat.MessageRoleSystem, result[0].Role)
			assert.Equal(t, compaction.SystemPrompt, result[0].Content)

			last := result[len(result)-1]
			assert.Equal(t, chat.MessageRoleUser, last.Role)
			expectedPrompt := compaction.UserPrompt
			if tt.additionalPrompt != "" {
				expectedPrompt += "\n\n" + tt.additionalPrompt
			}
			assert.Equal(t, expectedPrompt, last.Content)

			// Conversation messages are all except first (system) and last (user prompt)
			assert.Len(t, result[1:len(result)-1], tt.wantConversationMsgCount)

			// Verify cost and cache control are cleared on conversation messages
			for i := 1; i < len(result)-1; i++ {
				assert.Zero(t, result[i].Cost)
				assert.False(t, result[i].CacheControl)
			}
		})
	}
}

func TestExtractMessages_KeepsRecentMessages(t *testing.T) {
	t.Parallel()

	// Create a session with many messages, some large enough that the last
	// ~MaxKeepTokens are kept aside.
	var items []session.Item
	for range 10 {
		items = append(items, session.NewMessageItem(&session.Message{
			Message: chat.Message{
				Role:    chat.MessageRoleUser,
				Content: strings.Repeat("x", 20000), // ~5k tokens each
			},
		}), session.NewMessageItem(&session.Message{
			Message: chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: strings.Repeat("y", 20000), // ~5k tokens each
			},
		}))
	}

	sess := session.New(session.WithMessages(items))
	a := agent.New("test", "test prompt")

	result, firstKeptEntry := extractMessages(sess, a, 200_000, "")

	// 20 messages × ~5k tokens = ~100k. maxKeepTokens=20k → ~4 messages kept.
	compactedMsgCount := len(result) - 2 // minus system and user prompt
	assert.Less(t, compactedMsgCount, 20, "some messages should have been kept aside")
	assert.Positive(t, compactedMsgCount, "some messages should be compacted")

	assert.Positive(t, firstKeptEntry, "firstKeptEntry should be > 0")
	assert.Less(t, firstKeptEntry, len(sess.Messages), "firstKeptEntry should be within bounds")
}

func TestComputeFirstKeptEntry(t *testing.T) {
	t.Parallel()

	t.Run("empty session returns 0", func(t *testing.T) {
		t.Parallel()
		sess := session.New()
		assert.Equal(t, 0, ComputeFirstKeptEntry(sess))
	})

	t.Run("short conversation: split at end (compact everything)", func(t *testing.T) {
		t.Parallel()
		sess := session.New(session.WithMessages([]session.Item{
			session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleSystem, Content: "sys"}}),
			session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "hi"}}),
			session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "hello"}}),
		}))
		assert.Equal(t, len(sess.Messages), ComputeFirstKeptEntry(sess))
	})
}

func TestGatherCompactionInput_NoPriorSummary(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithMessages([]session.Item{
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleSystem, Content: "sys"}}),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "u1"}}),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "a1"}}),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleSystem, Content: "sys2"}}),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "u2"}}),
	}))

	messages, sessIndices := gatherCompactionInput(sess)
	require.Len(t, messages, 3)
	assert.Equal(t, []int{1, 2, 4}, sessIndices)

	assert.Equal(t, 1, firstKeptSessionIndex(sess, sessIndices, 0))
	assert.Equal(t, 2, firstKeptSessionIndex(sess, sessIndices, 1))
	assert.Equal(t, 4, firstKeptSessionIndex(sess, sessIndices, 2))
	// Past the end: returns len(sess.Messages) (compact-everything sentinel).
	assert.Equal(t, len(sess.Messages), firstKeptSessionIndex(sess, sessIndices, 3))
}

// TestGatherCompactionInput_WithPriorSummary pins the regression where
// an existing summary in the history made the runtime miscompute
// FirstKeptEntry: counting non-system items from index 0 ignores both
// the synthetic "Session Summary" message that surfaces at the head of
// the chat list and the prior summary's start offset, so the kept
// boundary lands far too early in the session.
func TestGatherCompactionInput_WithPriorSummary(t *testing.T) {
	t.Parallel()

	newMsgItem := func(role chat.MessageRole, content string) session.Item {
		return session.NewMessageItem(&session.Message{Message: chat.Message{Role: role, Content: content}})
	}

	// Session shape:
	//   [0..7]  : pre-compaction conversation (already summarized).
	//   [8..9]  : kept tail of the prior compaction (FirstKeptEntry=8).
	//   [10]    : prior summary item.
	//   [11..14]: post-compaction conversation.
	items := []session.Item{
		newMsgItem(chat.MessageRoleUser, "u0"),
		newMsgItem(chat.MessageRoleAssistant, "a0"),
		newMsgItem(chat.MessageRoleUser, "u1"),
		newMsgItem(chat.MessageRoleAssistant, "a1"),
		newMsgItem(chat.MessageRoleUser, "u2"),
		newMsgItem(chat.MessageRoleAssistant, "a2"),
		newMsgItem(chat.MessageRoleUser, "u3"),
		newMsgItem(chat.MessageRoleAssistant, "a3"),
		newMsgItem(chat.MessageRoleUser, "u4-kept"),
		newMsgItem(chat.MessageRoleAssistant, "a4-kept"),
		{Summary: "prior summary", FirstKeptEntry: 8},
		newMsgItem(chat.MessageRoleUser, "u5"),
		newMsgItem(chat.MessageRoleAssistant, "a5"),
		newMsgItem(chat.MessageRoleUser, "u6"),
		newMsgItem(chat.MessageRoleAssistant, "a6"),
	}
	sess := session.New(session.WithMessages(items))

	messages, sessIndices := gatherCompactionInput(sess)

	// Expected filtered list:
	//   [0]: synthetic Session Summary user message (origin: prior summary at idx 10)
	//   [1]: items[8]   (kept-tail user)
	//   [2]: items[9]   (kept-tail assistant)
	//   [3]: items[11]  (post-summary user)
	//   [4]: items[12]  (post-summary assistant)
	//   [5]: items[13]
	//   [6]: items[14]
	require.Len(t, messages, 7)
	assert.Equal(t, chat.MessageRoleUser, messages[0].Role)
	assert.Contains(t, messages[0].Content, "Session Summary: prior summary")
	assert.Equal(t, []int{10, 8, 9, 11, 12, 13, 14}, sessIndices)

	// A split that keeps the last two messages should map to items[13]
	// (the user message at idx 13), not to items[5] which is what the
	// old count-from-zero implementation produced.
	assert.Equal(t, 13, firstKeptSessionIndex(sess, sessIndices, 5))

	// A split that keeps the entire post-summary tail (everything from
	// items[8] onwards including the prior summary) maps the synthetic
	// message back to its originating summary index so the prior
	// summary item is preserved across the new compaction.
	assert.Equal(t, 10, firstKeptSessionIndex(sess, sessIndices, 0))

	// Out-of-range split: compact everything, keep nothing.
	assert.Equal(t, len(sess.Messages), firstKeptSessionIndex(sess, sessIndices, len(messages)))
}

// TestFirstKeptSessionIndex_SplitZeroOnEmptyInputUsesSafeSentinel
// pins the only path through which splitIdx == 0 can reach
// firstKeptSessionIndex: an empty messages list (which only happens
// for a brand-new session with no prior summary). In that case
// sessIndices is also empty and the out-of-range branch returns
// len(sess.Messages), the "compact everything; keep nothing" sentinel
// that session.buildSessionSummaryMessages safely treats as no kept
// tail.
//
// This is the safety net behind the
// SplitIndexForKeep_NeverReturnsZeroForNonEmptyInput invariant: even
// if a future change accidentally let splitIdx==0 escape from a
// non-empty SplitIndexForKeep call, the bot's concern ("sessIndices[0]
// = lastSummaryIdx is returned, dropping the prior kept-tail in the
// next reconstruction") only triggers when sessIndices is non-empty
// AND splitIdx==0 — which the invariant rules out and this test pins
// the empty-input alternative for.
func TestFirstKeptSessionIndex_SplitZeroOnEmptyInputUsesSafeSentinel(t *testing.T) {
	t.Parallel()

	sess := session.New()
	var sessIndices []int

	// Empty input is the only legitimate way splitIdx==0 reaches
	// firstKeptSessionIndex. Both branches (>= len(sessIndices) and
	// the indexed lookup) must yield len(sess.Messages) here.
	assert.Equal(t, len(sess.Messages), firstKeptSessionIndex(sess, sessIndices, 0))
}

// TestGatherCompactionInput_PriorSummaryWithoutFirstKeptEntry covers
// the case where a prior summary was applied as "compact everything,
// keep nothing" (FirstKeptEntry left at zero): the iteration must
// start strictly after the summary item, not from the top of the
// session.
func TestGatherCompactionInput_PriorSummaryWithoutFirstKeptEntry(t *testing.T) {
	t.Parallel()

	newMsgItem := func(role chat.MessageRole, content string) session.Item {
		return session.NewMessageItem(&session.Message{Message: chat.Message{Role: role, Content: content}})
	}

	items := []session.Item{
		newMsgItem(chat.MessageRoleUser, "old"),
		newMsgItem(chat.MessageRoleAssistant, "old-reply"),
		{Summary: "prior summary"},
		newMsgItem(chat.MessageRoleUser, "new"),
		newMsgItem(chat.MessageRoleAssistant, "new-reply"),
	}
	sess := session.New(session.WithMessages(items))

	messages, sessIndices := gatherCompactionInput(sess)

	// Filtered list: synthetic-summary, items[3], items[4].
	// items[0..1] are excluded because they were compacted into the
	// prior summary and FirstKeptEntry is zero.
	require.Len(t, messages, 3)
	assert.Equal(t, []int{2, 3, 4}, sessIndices)
}

func TestRunLLM_DoesNotDuplicateSystemPrompt(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithMessages([]session.Item{
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "please summarize"}}),
	}))
	a := agent.New("test", "parent prompt", agent.WithModel(fakeProvider{id: modelsdev.NewID("fake", "model")}))

	var systemPromptCount int
	result, err := RunLLM(t.Context(), LLMArgs{
		Session:      sess,
		Agent:        a,
		ContextLimit: 100_000,
		RunAgent: func(_ context.Context, compactionAgent *agent.Agent, compactionSession *session.Session) error {
			for _, msg := range compactionSession.GetMessages(compactionAgent) {
				if msg.Role == chat.MessageRoleSystem && msg.Content == compaction.SystemPrompt {
					systemPromptCount++
				}
			}
			compactionSession.AddMessage(&session.Message{Message: chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "summary",
			}})
			return nil
		},
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 1, systemPromptCount, "compaction sub-run should see the compaction system prompt exactly once")
}

// TestRunLLM_RequiresRunAgent pins the contract that a missing RunAgent
// callback is rejected loudly rather than silently no-oping.
func TestRunLLM_RequiresRunAgent(t *testing.T) {
	t.Parallel()

	sess := session.New()
	a := agent.New("test", "test")

	_, err := RunLLM(t.Context(), LLMArgs{
		Session:      sess,
		Agent:        a,
		ContextLimit: 100_000,
		// RunAgent intentionally nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RunAgent")
}

// TestRunLLM_RequiresContextLimit pins that the LLM strategy refuses
// to run without a real context budget — it would otherwise feed an
// empty conversation to the model.
func TestRunLLM_RequiresContextLimit(t *testing.T) {
	t.Parallel()

	sess := session.New()
	a := agent.New("test", "test")

	_, err := RunLLM(t.Context(), LLMArgs{
		Session:      sess,
		Agent:        a,
		ContextLimit: 0,
		RunAgent: func(context.Context, *agent.Agent, *session.Session) error {
			return errors.New("should not be called")
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ContextLimit")
}
