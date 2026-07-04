package compactor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// Scenario: second-round compaction over a session that already contains a
// summary with a kept tail. CompactionInput's sessIndices are
// non-monotonic in this case (synthetic summary -> old kept tail ->
// post-summary conversation); verify that the FirstKeptEntry computed
// for the SECOND summary partitions the conversation with no message
// both summarized and kept, and none lost.
func TestScenario_SecondRoundCompaction_PartitionIsExact(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("x", 35_000) // ~10k tokens each
	msg := func(role chat.MessageRole, content string) session.Item {
		return session.NewMessageItem(&session.Message{Message: chat.Message{Role: role, Content: content}})
	}

	items := []session.Item{
		msg(chat.MessageRoleUser, "m0"),             // 0: folded into old summary
		msg(chat.MessageRoleAssistant, "m1"),        // 1: folded into old summary
		msg(chat.MessageRoleUser, "m2"),             // 2: old kept tail
		msg(chat.MessageRoleAssistant, "m3"),        // 3: old kept tail
		{Summary: "old summary", FirstKeptEntry: 2}, // 4: prior compaction
		msg(chat.MessageRoleUser, big+"m5"),         // 5
		msg(chat.MessageRoleAssistant, big+"m6"),    // 6
		msg(chat.MessageRoleUser, big+"m7"),         // 7
		msg(chat.MessageRoleAssistant, "m8"),        // 8
	}
	sess := session.New(session.WithMessages(items))

	messages, sessIndices := gatherCompactionInput(sess)
	// synthetic summary(->4), m2(->2), m3(->3), m5..m8(->5..8)
	require.Equal(t, []int{4, 2, 3, 5, 6, 7, 8}, sessIndices)
	require.Contains(t, messages[0].Content, "Session Summary: old summary")

	// Keep budget of 20k (contextLimit 100k) keeps roughly the last two
	// big messages; everything else is summarized.
	firstKept := ComputeFirstKeptEntry(sess, 100_000)
	require.Greater(t, firstKept, 4, "kept tail should start after the old summary item")
	require.Less(t, firstKept, len(items), "something should be kept")

	// Simulate the runtime applying the new summary.
	sess.ApplyCompaction(100, 0, session.Item{Summary: "new summary", FirstKeptEntry: firstKept})

	// Reconstruct what the next prompt will contain via a third
	// CompactionInput round (mirrors buildSessionSummaryMessages).
	after, afterIdx := gatherCompactionInput(sess)
	require.Contains(t, after[0].Content, "Session Summary: new summary")

	// Every original message must be either folded (index < firstKept in
	// the kept-tail sense) or kept verbatim — never both, never lost.
	keptContents := map[string]bool{}
	for _, m := range after[1:] {
		keptContents[m.Content] = true
	}
	for i := firstKept; i < len(items)-1; i++ { // -1: the new summary item itself
		if items[i].IsMessage() {
			assert.True(t, keptContents[items[i].Message.Message.Content],
				"message at session index %d must be kept verbatim", i)
		}
	}
	for _, idx := range afterIdx[1:] {
		assert.GreaterOrEqual(t, idx, firstKept,
			"no message before FirstKeptEntry may leak into the reconstructed prompt")
	}
}

// Scenario: manual /compact on a tiny session (everything fits the keep
// budget). SplitIndexForKeep returns len(messages) -> everything is
// summarized, nothing kept. Verify the sentinel maps to
// len(sess.Messages) and doesn't panic or point at a bogus index.
func TestScenario_TinySessionCompactsEverything(t *testing.T) {
	t.Parallel()

	items := []session.Item{
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "hi"}}),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "hello"}}),
	}
	sess := session.New(session.WithMessages(items))
	firstKept := ComputeFirstKeptEntry(sess, 100_000)
	assert.Equal(t, len(sess.Messages), firstKept, "compact-everything sentinel")
}

// Scenario: messages appended between the FirstKeptEntry snapshot and
// ApplyCompaction (steered input racing a compaction) must survive:
// they land inside the kept-tail window, never inside the summarized
// range.
func TestScenario_MessagesAppendedDuringCompactionAreKept(t *testing.T) {
	t.Parallel()

	items := []session.Item{
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "hi"}}),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "hello"}}),
	}
	sess := session.New(session.WithMessages(items))

	// Snapshot: compact everything (sentinel = len at snapshot time).
	firstKept := ComputeFirstKeptEntry(sess, 100_000)
	require.Equal(t, 2, firstKept)

	// A steered user message races in before the summary is applied.
	sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "raced steer"}})
	sess.ApplyCompaction(10, 0, session.Item{Summary: "sum", FirstKeptEntry: firstKept})

	msgs, _ := gatherCompactionInput(sess)
	require.Len(t, msgs, 2, "summary + raced message")
	assert.Contains(t, msgs[0].Content, "Session Summary: sum")
	assert.Equal(t, "raced steer", msgs[1].Content, "the raced message must be preserved verbatim")
}

// Scenario: a prior summary at the very front of the summarizer input is
// dropped when the summarization budget is too tight — the new summary
// then loses everything the old one contained. Documents current
// (lossy) behavior.
func TestScenario_TightBudgetDropsPriorSummaryFromInput(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("x", 40_000) // ~11.4k tokens
	items := []session.Item{
		{Summary: "IMPORTANT old facts", FirstKeptEntry: 0},
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: big + "recent1"}}),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: big + "recent2"}}),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: big + "recent3"}}),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "tail"}}),
	}
	sess := session.New(session.WithMessages(items))

	// context 16k: summary budget 4k, keep budget 3.2k, available ~11.8k.
	msgs, _ := extractMessages(sess, nil, 16_000, "")

	var sawOldSummary bool
	for _, m := range msgs {
		if strings.Contains(m.Content, "IMPORTANT old facts") {
			sawOldSummary = true
		}
	}
	// Current behavior: front-truncation drops the prior summary. If this
	// assertion starts failing, the lossy behavior was fixed — update the
	// probe.
	assert.False(t, sawOldSummary,
		"documents current behavior: tight budgets silently drop the prior summary from the summarizer input")
}
