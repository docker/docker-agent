package compactor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

type fakeProvider struct{ id modelsdev.ID }

func (p fakeProvider) ID() modelsdev.ID { return p.id }

func (p fakeProvider) BaseConfig() base.Config {
	return base.Config{RebuildProvider: func(context.Context, *latest.ModelConfig, ...options.Opt) (provider.Provider, error) { return p, nil }}
}

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
			name: "older messages dropped when they exceed the summarization budget",
			messages: []session.Item{
				newMsg(chat.MessageRoleUser, strings.Repeat("a", 80_000)),      // ~20k tokens
				newMsg(chat.MessageRoleAssistant, strings.Repeat("b", 80_000)), // ~20k tokens
				newMsg(chat.MessageRoleUser, "second message"),
				newMsg(chat.MessageRoleAssistant, "second response"),
			},
			// The two small messages form the kept tail (keep budget
			// 32k/5). Of the two ~20k-token compact candidates only the
			// newest fits contextAvailable ≈ 0.75×32k − prompts ≈ 23.8k;
			// the older one is dropped from the summarizer's input.
			contextLimit:             32_000,
			wantConversationMsgCount: 1,
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
			result, _, _ := extractMessages(sess, a, tt.contextLimit, tt.additionalPrompt, 0)

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
	// ~maxKeepTokens are kept aside.
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

	result, firstKeptEntry, _ := extractMessages(sess, a, 200_000, "", 0)

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
		assert.Equal(t, 0, ComputeFirstKeptEntry(sess, 100_000))
	})

	t.Run("short conversation: split at end (compact everything)", func(t *testing.T) {
		t.Parallel()
		sess := session.New(session.WithMessages([]session.Item{
			session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleSystem, Content: "sys"}}),
			session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "hi"}}),
			session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "hello"}}),
		}))
		assert.Equal(t, len(sess.Messages), ComputeFirstKeptEntry(sess, 100_000))
	})
}

func TestLastAssistantContentAfter(t *testing.T) {
	t.Parallel()

	newMsg := func(role chat.MessageRole, content string) session.Item {
		return session.NewMessageItem(&session.Message{
			Message: chat.Message{Role: role, Content: content},
		})
	}

	tests := []struct {
		name     string
		messages []session.Item
		seedLen  int
		want     string
	}{
		{
			name:     "no messages after seed",
			messages: []session.Item{newMsg(chat.MessageRoleAssistant, "seed reply")},
			seedLen:  1,
			want:     "",
		},
		{
			name: "seed assistant reply is never picked up",
			messages: []session.Item{
				newMsg(chat.MessageRoleAssistant, "stale reply"),
				newMsg(chat.MessageRoleUser, "summarize"),
			},
			seedLen: 2,
			want:    "",
		},
		{
			name: "summary is trimmed",
			messages: []session.Item{
				newMsg(chat.MessageRoleUser, "summarize"),
				newMsg(chat.MessageRoleAssistant, "  summary \n"),
			},
			seedLen: 1,
			want:    "summary",
		},
		{
			name: "whitespace-only trailing reply does not hide an earlier summary",
			messages: []session.Item{
				newMsg(chat.MessageRoleUser, "summarize"),
				newMsg(chat.MessageRoleAssistant, "real summary"),
				newMsg(chat.MessageRoleTool, "tool result"),
				newMsg(chat.MessageRoleAssistant, "\n \t"),
			},
			seedLen: 1,
			want:    "real summary",
		},
		{
			name: "all whitespace-only replies yield no summary",
			messages: []session.Item{
				newMsg(chat.MessageRoleUser, "summarize"),
				newMsg(chat.MessageRoleAssistant, " "),
				newMsg(chat.MessageRoleAssistant, "\n"),
			},
			seedLen: 1,
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sess := session.New(session.WithMessages(tt.messages))
			assert.Equal(t, tt.want, lastAssistantContentAfter(sess, tt.seedLen))
		})
	}
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

	messages, sessIndices, itemCount := sess.CompactionInput()
	require.Len(t, messages, 3)
	assert.Equal(t, []int{1, 2, 4}, sessIndices)

	assert.Equal(t, 1, firstKeptSessionIndex(sessIndices, itemCount, 0))
	assert.Equal(t, 2, firstKeptSessionIndex(sessIndices, itemCount, 1))
	assert.Equal(t, 4, firstKeptSessionIndex(sessIndices, itemCount, 2))
	// Past the end: returns len(sess.Messages) (compact-everything sentinel).
	assert.Equal(t, len(sess.Messages), firstKeptSessionIndex(sessIndices, itemCount, 3))
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

	messages, sessIndices, itemCount := sess.CompactionInput()

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
	assert.Equal(t, 13, firstKeptSessionIndex(sessIndices, itemCount, 5))

	// A split that keeps the entire post-summary tail (everything from
	// items[8] onwards including the prior summary) maps the synthetic
	// message back to its originating summary index so the prior
	// summary item is preserved across the new compaction.
	assert.Equal(t, 10, firstKeptSessionIndex(sessIndices, itemCount, 0))

	// Out-of-range split: compact everything, keep nothing.
	assert.Equal(t, len(sess.Messages), firstKeptSessionIndex(sessIndices, itemCount, len(messages)))
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
	itemCount := sess.ItemCount()

	// Empty input is the only legitimate way splitIdx==0 reaches
	// firstKeptSessionIndex. Both branches (>= len(sessIndices) and
	// the indexed lookup) must yield itemCount here.
	assert.Equal(t, itemCount, firstKeptSessionIndex(sessIndices, itemCount, 0))
}

// TestGatherCompactionInputConcurrent extends session's own
// TestCompactionInputConcurrent (pkg/session/session_race_test.go) to the
// compactor's own boundary computation: session.CompactionInput plus
// firstKeptSessionIndex must stay race-free when called concurrently with
// AddMessage on the same live session. firstKeptSessionIndex itself no
// longer touches the session (it now takes the snapshot's own itemCount
// instead of calling sess.ItemCount() — see
// TestGatherCompactionInput_OutOfRangeSentinelMatchesSnapshotCount below for
// why), so the only thing left to prove race-free here is
// CompactionInput's snapshot read itself.
func TestGatherCompactionInputConcurrent(t *testing.T) {
	t.Parallel()

	sess := session.New()
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			sess.AddMessage(session.UserMessage("u"))
		})
		wg.Go(func() {
			_, sessIndices, itemCount := sess.CompactionInput()
			_ = firstKeptSessionIndex(sessIndices, itemCount, 0)
		})
	}
	wg.Wait()
}

// TestGatherCompactionInput_OutOfRangeSentinelMatchesSnapshotCount pins the
// other #3590 blocker: the out-of-range sentinel returned by
// firstKeptSessionIndex must describe the SAME snapshot CompactionInput
// produced sessIndices from, not whatever sess.ItemCount() returns when read
// later. A reviewer probe demonstrated the bug directly: the sentinel
// described a session length one longer (200001) than the snapshot it was
// supposedly derived from (200000) once a message was appended in between.
// This reproduces that shape deterministically — no goroutines or -race
// needed, since the bug was a logic error (reading two different sources of
// truth), not a data race.
func TestGatherCompactionInput_OutOfRangeSentinelMatchesSnapshotCount(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithMessages([]session.Item{
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "u1"}}),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "a1"}}),
	}))

	_, sessIndices, itemCount := sess.CompactionInput()
	snapshotCount := len(sess.Messages)
	require.Equal(t, snapshotCount, itemCount)

	// A message "races in" after the snapshot was taken, e.g. a concurrent
	// HTTP AddMessage landing mid-compaction.
	sess.AddMessage(session.UserMessage("late arrival"))
	require.Equal(t, snapshotCount+1, sess.ItemCount(), "sanity: the live session grew past the snapshot")

	// The out-of-range sentinel must still describe the snapshot count, not
	// the now-longer live session.
	got := firstKeptSessionIndex(sessIndices, itemCount, len(sessIndices))
	assert.Equal(t, snapshotCount, got)
	assert.NotEqual(t, sess.ItemCount(), got)
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

	messages, sessIndices, _ := sess.CompactionInput()

	// Filtered list: synthetic-summary, items[3], items[4].
	// items[0..1] are excluded because they were compacted into the
	// prior summary and FirstKeptEntry is zero.
	require.Len(t, messages, 3)
	assert.Equal(t, []int{2, 3, 4}, sessIndices)
}

// TestRunLLM_SmallContextWindow is a regression test for issue #2871:
// with a small context window (e.g. a local model whose size comes from
// provider_opts.context_size), the fixed MaxSummaryTokens budget used to
// consume the whole window, so the summarizer received zero conversation
// messages, fabricated an "I see no conversation history" reply, and that
// text then replaced the entire session history. The budgets must scale
// with the window so the summarizer always sees real conversation.
func TestRunLLM_SmallContextWindow(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("x", 4_000) // ~1k estimated tokens per tool result
	sess := session.New(session.WithUserMessage("please do the big task"))
	for i := range 8 {
		id := fmt.Sprintf("tc%d", i)
		sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
			Role:      chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{{ID: id, Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"ls"}`}}},
		}))
		sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
			Role:       chat.MessageRoleTool,
			ToolCallID: id,
			Content:    big,
		}))
	}
	a := agent.New("root", "instr", agent.WithModel(fakeProvider{id: modelsdev.NewID("fake", "model")}))

	var conversationCount int
	result, err := RunLLM(t.Context(), LLMArgs{
		Session:      sess,
		Agent:        a,
		ContextLimit: 8_192,
		RunAgent: func(_ context.Context, _ *agent.Agent, cs *session.Session) error {
			msgs := cs.GetAllMessages()
			// All non-system messages minus the trailing compaction user
			// prompt are the conversation handed to the summarizer.
			conversationCount = len(msgs) - 1
			cs.AddMessage(session.NewAgentMessage("root", &chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "the summary",
			}))
			return nil
		},
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Positive(t, conversationCount, "summarizer must receive conversation messages even on small context windows")
	assert.Equal(t, "the summary", result.Summary)
	assert.Less(t, result.FirstKeptEntry, len(sess.Messages), "a recent tail must be kept verbatim")
	assert.Positive(t, result.FirstKeptEntry)
}

// TestRunLLM_ReportsModelAndUsage verifies the summarization run's model and
// token usage travel on the Result, so the summary's cost can be attributed
// per model (e.g. in the /cost dialog).
func TestRunLLM_ReportsModelAndUsage(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithUserMessage("please do the task"))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "done",
	}))
	a := agent.New("root", "instr", agent.WithModel(fakeProvider{id: modelsdev.NewID("fake", "model")}))

	result, err := RunLLM(t.Context(), LLMArgs{
		Session:      sess,
		Agent:        a,
		ContextLimit: 8_192,
		RunAgent: func(_ context.Context, _ *agent.Agent, cs *session.Session) error {
			cs.AddMessage(session.NewAgentMessage("root", &chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "the summary",
				Model:   "fake/model",
				Usage:   &chat.Usage{InputTokens: 123, OutputTokens: 45},
			}))
			return nil
		},
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "fake/model", result.Model)
	assert.Equal(t, int64(123), result.Usage.InputTokens)
	assert.Equal(t, int64(45), result.Usage.OutputTokens)
}

// TestRunLLM_CompactionSessionInheritsWorkspace verifies the ephemeral
// summarization session runs with the compacted session's workspace root.
func TestRunLLM_CompactionSessionInheritsWorkspace(t *testing.T) {
	t.Parallel()

	sess := session.New(
		session.WithUserMessage("please do the task"),
		session.WithWorkingDir("/work/project"),
	)
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "done",
	}))
	a := agent.New("root", "instr", agent.WithModel(fakeProvider{id: modelsdev.NewID("fake", "model")}))

	_, err := RunLLM(t.Context(), LLMArgs{
		Session:      sess,
		Agent:        a,
		ContextLimit: 8_192,
		RunAgent: func(_ context.Context, _ *agent.Agent, cs *session.Session) error {
			assert.Equal(t, "/work/project", cs.WorkingDir)
			cs.AddMessage(session.NewAgentMessage("root", &chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "the summary",
			}))
			return nil
		},
	})
	require.NoError(t, err)
}

// TestRunLLM_NoConversationFits_NoOps pins the safety net behind the
// scaled budgets: when not a single conversation message fits the
// summarization budget (e.g. one giant tool result), RunLLM must no-op
// instead of running the summarizer on an empty conversation — the
// resulting non-summary would otherwise wipe the session history.
func TestRunLLM_NoConversationFits_NoOps(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithMessages([]session.Item{
		session.NewMessageItem(&session.Message{Message: chat.Message{
			Role:    chat.MessageRoleUser,
			Content: strings.Repeat("x", 200_000), // ~50k tokens, exceeds the whole window
		}}),
	}))
	a := agent.New("root", "instr", agent.WithModel(fakeProvider{id: modelsdev.NewID("fake", "model")}))

	runAgentCalled := false
	result, err := RunLLM(t.Context(), LLMArgs{
		Session:      sess,
		Agent:        a,
		ContextLimit: 8_192,
		RunAgent: func(context.Context, *agent.Agent, *session.Session) error {
			runAgentCalled = true
			return nil
		},
	})

	require.NoError(t, err)
	assert.Nil(t, result, "compaction must be a no-op when nothing fits the budget")
	assert.False(t, runAgentCalled, "the summarizer must not run on an empty conversation")
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

// TestRunLLM_ModelSelection pins that the summary is generated by the
// dedicated compaction model when one is configured on the agent, and falls
// back to the agent's own model otherwise (issue #3241).
func TestRunLLM_ModelSelection(t *testing.T) {
	t.Parallel()

	newSession := func() *session.Session {
		return session.New(session.WithMessages([]session.Item{
			session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "please summarize"}}),
		}))
	}

	run := func(t *testing.T, a *agent.Agent) string {
		t.Helper()
		var summaryModelID string
		result, err := RunLLM(t.Context(), LLMArgs{
			Session:      newSession(),
			Agent:        a,
			ContextLimit: 100_000,
			RunAgent: func(ctx context.Context, compactionAgent *agent.Agent, cs *session.Session) error {
				summaryModelID = compactionAgent.Model(ctx).ID().String()
				cs.AddMessage(session.NewAgentMessage("root", &chat.Message{
					Role:    chat.MessageRoleAssistant,
					Content: "the summary",
				}))
				return nil
			},
		})
		require.NoError(t, err)
		require.NotNil(t, result)
		return summaryModelID
	}

	t.Run("uses dedicated compaction model", func(t *testing.T) {
		t.Parallel()
		a := agent.New("root", "instr",
			agent.WithModel(fakeProvider{id: modelsdev.NewID("primary", "big")}),
			agent.WithCompactionModel(fakeProvider{id: modelsdev.NewID("compaction", "small")}),
		)
		assert.Equal(t, "compaction/small", run(t, a))
	})

	t.Run("falls back to agent's own model", func(t *testing.T) {
		t.Parallel()
		a := agent.New("root", "instr",
			agent.WithModel(fakeProvider{id: modelsdev.NewID("primary", "big")}),
		)
		assert.Equal(t, "primary/big", run(t, a))
	})
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

type failedCloneModel struct {
	base.Config
}

func (p *failedCloneModel) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	panic("base inference must not run")
}

func TestRunLLMCloneFailureLeavesSessionIntact(t *testing.T) {
	t.Parallel()
	synthetic := errors.New("synthetic reconstruction failure")
	for _, failure := range []error{nil, synthetic} {
		p := &failedCloneModel{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "test", Model: "broken"}}}
		if failure != nil {
			p.RebuildProvider = func(context.Context, *latest.ModelConfig, ...options.Opt) (provider.Provider, error) {
				return nil, failure
			}
		}
		sess := session.New(session.WithMessages([]session.Item{session.NewMessageItem(session.UserMessage("keep history"))}))
		before := sess.MessagesSnapshot()
		result, err := RunLLM(t.Context(), LLMArgs{Session: sess, Agent: agent.New("root", "", agent.WithModel(p)), ContextLimit: 100_000, RunAgent: func(context.Context, *agent.Agent, *session.Session) error {
			t.Error("summary runtime must not run")
			return nil
		}})
		want := failure
		if want == nil {
			want = provider.ErrNoRebuilder
		}
		require.ErrorIs(t, err, want)
		assert.Nil(t, result)
		assert.Equal(t, before, sess.MessagesSnapshot())
	}
}

func overflowTestSession() *session.Session {
	big := strings.Repeat("x", 7_000)
	sess := session.New()
	for i := range 20 {
		sess.AddMessage(session.UserMessage(fmt.Sprintf("step %d", i)))
		sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: big}))
	}
	return sess
}

func conversationTokens(cs *session.Session) int64 {
	msgs := cs.GetAllMessages()
	var n int64
	for i := range msgs {
		m := msgs[i].Message
		if m.Role == chat.MessageRoleSystem || i == len(msgs)-1 {
			continue
		}
		n += compaction.EstimateMessageTokens(&m)
	}
	return n
}

// TestRunLLM_RetriesSmallerAfterSummaryOverflow covers a serving window
// smaller than ContextLimit: the summary request overflows and must be
// retried with strictly less conversation until it fits.
func TestRunLLM_RetriesSmallerAfterSummaryOverflow(t *testing.T) {
	t.Parallel()

	const window = 8_000
	sess := overflowTestSession()
	a := agent.New("root", "instr", agent.WithModel(fakeProvider{id: modelsdev.NewID("fake", "model")}))

	var sizes []int64
	result, err := RunLLM(t.Context(), LLMArgs{
		Session:      sess,
		Agent:        a,
		ContextLimit: 1_000_000,
		RunAgent: func(_ context.Context, _ *agent.Agent, cs *session.Session) error {
			size := conversationTokens(cs)
			sizes = append(sizes, size)
			if size > window {
				cs.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Cost: 0.25, Usage: &chat.Usage{InputTokens: 7}}))
				return modelerrors.NewContextOverflowError(errors.New("maximum context length exceeded"))
			}
			cs.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "the summary", Cost: 1, Usage: &chat.Usage{InputTokens: 100}}))
			return nil
		},
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "the summary", result.Summary)
	require.Greater(t, len(sizes), 1)
	for i := 1; i < len(sizes); i++ {
		assert.LessOrEqual(t, sizes[i], sizes[i-1]/2, "each retry must halve the conversation")
	}
	assert.LessOrEqual(t, sizes[len(sizes)-1], int64(window))
	failed := len(sizes) - 1
	assert.InDelta(t, 1+0.25*float64(failed), result.Cost, 1e-9, "failed attempts' cost must not be dropped")
	assert.Equal(t, int64(100+7*failed), result.Usage.InputTokens)
}

// TestRunLLM_SummaryOverflowRetriesAreBounded verifies a summarizer that
// keeps overflowing fails with the overflow error after a bounded number of
// attempts instead of looping or silently skipping.
func TestRunLLM_SummaryOverflowRetriesAreBounded(t *testing.T) {
	t.Parallel()

	sess := overflowTestSession()
	a := agent.New("root", "instr", agent.WithModel(fakeProvider{id: modelsdev.NewID("fake", "model")}))

	calls := 0
	result, err := RunLLM(t.Context(), LLMArgs{
		Session:      sess,
		Agent:        a,
		ContextLimit: 1_000_000,
		RunAgent: func(context.Context, *agent.Agent, *session.Session) error {
			calls++
			return modelerrors.NewContextOverflowError(errors.New("maximum context length exceeded"))
		},
	})

	require.Error(t, err)
	assert.Nil(t, result)
	assert.Equal(t, modelerrors.OverflowKindTokens, modelerrors.OverflowKindOf(err))
	assert.Equal(t, 1+maxSummaryOverflowRetries, calls)
	assert.Len(t, sess.Messages, 40, "a failed compaction must not modify the session")
}

// TestRunLLM_NonOverflowErrorIsNotRetried keeps non-overflow failures
// (auth, network, provider errors) on the single-attempt path.
func TestRunLLM_NonOverflowErrorIsNotRetried(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{"generic error", errors.New("boom")},
		{"wire overflow", &modelerrors.ContextOverflowError{Underlying: errors.New("request too large"), Kind: modelerrors.OverflowKindWire}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := agent.New("root", "instr", agent.WithModel(fakeProvider{id: modelsdev.NewID("fake", "model")}))
			calls := 0
			_, err := RunLLM(t.Context(), LLMArgs{
				Session:      overflowTestSession(),
				Agent:        a,
				ContextLimit: 1_000_000,
				RunAgent: func(context.Context, *agent.Agent, *session.Session) error {
					calls++
					return tt.err
				},
			})
			require.ErrorIs(t, err, tt.err)
			assert.Equal(t, 1, calls)
		})
	}
}

func priorSummarySession(summary string) *session.Session {
	big := strings.Repeat("x", 7_000)
	items := []session.Item{{Summary: summary}}
	for i := range 20 {
		items = append(items,
			session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: fmt.Sprintf("step %d", i)}}),
			session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: big}}),
		)
	}
	return session.New(session.WithMessages(items))
}

// TestRunLLM_OverflowRetryKeepsPriorSummary: retries front-truncate the
// conversation, which would drop the prior summary first and lose every
// fact it carries.
func TestRunLLM_OverflowRetryKeepsPriorSummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		summary  string
		wantKept bool
	}{
		{"summary fits the retry budget", "IMPORTANT old facts", true},
		// Larger than any retry budget: falls back to plain truncation
		// rather than sending an oversized request.
		{"summary larger than the retry budget", "IMPORTANT old facts " + strings.Repeat("y", 140_000), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sess := priorSummarySession(tt.summary)
			a := agent.New("root", "instr", agent.WithModel(fakeProvider{id: modelsdev.NewID("fake", "model")}))

			var attempts []bool
			result, err := RunLLM(t.Context(), LLMArgs{
				Session:      sess,
				Agent:        a,
				ContextLimit: 1_000_000,
				RunAgent: func(_ context.Context, _ *agent.Agent, cs *session.Session) error {
					var saw bool
					for _, m := range cs.GetAllMessages() {
						if strings.Contains(m.Message.Content, "IMPORTANT old facts") {
							saw = true
						}
					}
					attempts = append(attempts, saw)
					if len(attempts) == 1 {
						return modelerrors.NewContextOverflowError(errors.New("maximum context length exceeded"))
					}
					cs.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "the summary"}))
					return nil
				},
			})

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, attempts, 2)
			assert.True(t, attempts[0], "the first attempt sends the whole conversation")
			assert.Equal(t, tt.wantKept, attempts[1])
		})
	}
}

type timeoutCompactionProvider struct {
	cfg base.Config
}

func (p *timeoutCompactionProvider) ID() modelsdev.ID {
	return modelsdev.NewID(p.cfg.ModelConfig.Provider, p.cfg.ModelConfig.Model)
}

func (p *timeoutCompactionProvider) BaseConfig() base.Config { return p.cfg }

func (*timeoutCompactionProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, nil
}

func TestRunLLM_StreamIdleTimeout(t *testing.T) {
	t.Parallel()
	timeout := func(d time.Duration) *latest.StreamIdleTimeout { return &latest.StreamIdleTimeout{Duration: d} }
	for _, tc := range []struct {
		name      string
		file      *latest.StreamIdleTimeout
		model     *latest.StreamIdleTimeout
		dedicated bool
	}{
		{name: "unset"},
		{name: "parent model and file", file: timeout(time.Minute)},
		{name: "disabled file", file: timeout(latest.DisabledStreamIdleTimeout)},
		{name: "disabled parent model", file: timeout(time.Minute), model: timeout(latest.DisabledStreamIdleTimeout)},
		{name: "disabled compaction model", file: timeout(time.Minute), model: timeout(latest.DisabledStreamIdleTimeout), dedicated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var rebuild base.RebuildProviderFunc
			rebuild = func(_ context.Context, cfg *latest.ModelConfig, _ ...options.Opt) (provider.Provider, error) {
				return &timeoutCompactionProvider{cfg: base.Config{ModelConfig: *cfg, RebuildProvider: rebuild}}, nil
			}
			model := &timeoutCompactionProvider{cfg: base.Config{ModelConfig: latest.ModelConfig{Provider: "fake", Model: "summary", StreamIdleTimeout: tc.model}, RebuildProvider: rebuild}}
			opts := []agent.Opt{agent.WithModel(model), agent.WithStreamIdleTimeout(tc.file)}
			if tc.dedicated {
				opts = append(opts, agent.WithModel(fakeProvider{id: modelsdev.NewID("fake", "parent")}), agent.WithCompactionModel(model))
			}
			sess := session.New(session.WithMessages([]session.Item{
				session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "summarize"}}),
			}))
			result, err := RunLLM(t.Context(), LLMArgs{
				Session: sess, Agent: agent.New("root", "", opts...), ContextLimit: 100_000,
				RunAgent: func(ctx context.Context, child *agent.Agent, cs *session.Session) error {
					assert.Equal(t, tc.file, child.StreamIdleTimeout())
					assert.Equal(t, tc.model, child.Model(ctx).BaseConfig().ModelConfig.StreamIdleTimeout)
					cs.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "summary"}))
					return nil
				},
			})
			require.NoError(t, err)
			require.NotNil(t, result)
		})
	}
}
