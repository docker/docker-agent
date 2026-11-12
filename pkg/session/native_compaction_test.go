package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
)

func nativeCompaction() *chat.CompactionResult {
	return &chat.CompactionResult{
		Summary:  "native summary",
		Block:    json.RawMessage(`{"type":"compaction","content":"opaque"}`),
		Provider: "anthropic",
		Model:    "claude-test",
		Usage:    chat.Usage{InputTokens: 10, OutputTokens: 2},
	}
}

func TestItemCompactionJSONRoundTrip(t *testing.T) {
	t.Parallel()

	sess := New(WithID("json"), WithMessages([]Item{
		NewMessageItem(UserMessage("hi")),
		{Summary: "native summary", FirstKeptEntry: 1, Compaction: nativeCompaction()},
	}))

	data, err := json.Marshal(sess)
	require.NoError(t, err)
	var reloaded Session
	require.NoError(t, json.Unmarshal(data, &reloaded))

	require.Len(t, reloaded.Messages, 2)
	assert.Equal(t, sess.Messages[1].Compaction, reloaded.Messages[1].Compaction)
	assert.JSONEq(t, `{"type":"compaction","content":"opaque"}`, string(reloaded.Messages[1].Compaction.Block))
}

func TestPersistCompactionStoresNativeBlockAcrossStores(t *testing.T) {
	tests := []struct {
		name  string
		store func(*testing.T) Store
	}{
		{name: "memory", store: newInMemoryStoreForCompactionTest},
		{name: "sqlite", store: newSQLiteStoreForCompactionTest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := tt.store(t)
			sess := New(WithID("native"), WithMessages([]Item{NewMessageItem(UserMessage("hi"))}))
			require.NoError(t, store.AddSession(t.Context(), sess))

			item := Item{Summary: "native summary", FirstKeptEntry: 1, Compaction: nativeCompaction()}
			require.NoError(t, store.PersistCompaction(t.Context(), sess, 7, 0, item))
			require.NoError(t, store.AddSummary(t.Context(), sess.ID, Item{Summary: "plain summary"}))

			reloaded, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			require.Len(t, reloaded.Messages, 3)
			assert.Equal(t, nativeCompaction(), reloaded.Messages[1].Compaction)
			assert.Nil(t, reloaded.Messages[2].Compaction, "LLM summaries carry no native payload")
		})
	}
}

func TestSummaryMessageCarriesCompaction(t *testing.T) {
	t.Parallel()

	sess := New(WithMessages([]Item{
		NewMessageItem(UserMessage("old")),
		{Summary: "native summary", FirstKeptEntry: 1, Compaction: nativeCompaction()},
		NewMessageItem(UserMessage("new")),
	}))
	a := agent.New("test", "instruction")

	messages := sess.GetMessages(a)
	var summaryMsg *chat.Message
	for i := range messages {
		if messages[i].Compaction != nil {
			summaryMsg = &messages[i]
		}
	}
	require.NotNil(t, summaryMsg)
	assert.Equal(t, chat.MessageRoleUser, summaryMsg.Role)
	assert.Equal(t, SummaryMessageContent("native summary"), summaryMsg.Content, "text stays readable for non-native providers")
	assert.NotNil(t, summaryMsg.ReplayableCompaction("anthropic"))
	assert.Nil(t, summaryMsg.ReplayableCompaction("openai"))

	// The prompt copy must not alias the stored block.
	summaryMsg.Compaction.Block[0] = 'X'
	assert.Equal(t, byte('{'), sess.Messages[1].Compaction.Block[0])

	input, _, _ := sess.CompactionInput()
	require.NotEmpty(t, input)
	assert.Equal(t, SummaryMessageContent("native summary"), input[0].Content)
	assert.Nil(t, input[0].Compaction, "the LLM strategy summarizes the readable text, never a foreign signed block")
}

func TestEditedSummaryIsNotReplayed(t *testing.T) {
	t.Parallel()

	compaction := nativeCompaction()
	sess := New(WithMessages([]Item{
		{Summary: "edited by user", FirstKeptEntry: 0, Compaction: compaction},
	}))
	a := agent.New("test", "instruction")

	for _, msg := range sess.GetMessages(a) {
		if msg.Compaction == nil {
			continue
		}
		assert.Equal(t, SummaryMessageContent("edited by user"), msg.Content)
		assert.Nil(t, msg.ReplayableCompaction("anthropic"), "a summary edited after compaction must not resurrect the original block")
		return
	}
	t.Fatal("expected the prompt to carry the summary message")
}

func TestCloneAndForkDeepCopyCompaction(t *testing.T) {
	t.Parallel()

	sess := New(WithID("src"), WithMessages([]Item{
		NewMessageItem(UserMessage("old")),
		{Summary: "native summary", FirstKeptEntry: 1, Compaction: nativeCompaction()},
	}))

	clone := sess.Clone()
	forked, err := ForkSession(sess, 2)
	require.NoError(t, err)

	for name, copied := range map[string]*Session{"clone": clone, "fork": forked} {
		require.Len(t, copied.Messages, 2, name)
		require.Equal(t, nativeCompaction(), copied.Messages[1].Compaction, name)
		copied.Messages[1].Compaction.Block[0] = 'X'
	}
	assert.Equal(t, byte('{'), sess.Messages[1].Compaction.Block[0], "copies must not alias the source block")
}

func TestGetMessagesAndItemCountMatchesSnapshot(t *testing.T) {
	t.Parallel()

	sess := New(WithMessages([]Item{
		NewMessageItem(UserMessage("a")),
		NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "b"}}),
	}))
	a := agent.New("test", "instruction")

	messages, itemCount := sess.GetMessagesAndItemCount(a)
	assert.Equal(t, 2, itemCount)
	assert.Equal(t, sess.GetMessages(a), messages)

	// Recording itemCount as FirstKeptEntry keeps nothing of the snapshot
	// but preserves whatever was appended after it.
	sess.AddMessage(UserMessage("late"))
	sess.ApplyCompaction(1, 0, Item{Summary: "s", FirstKeptEntry: itemCount})
	var conversation []string
	for _, msg := range sess.GetMessages(a) {
		if msg.Role != chat.MessageRoleSystem {
			conversation = append(conversation, msg.Content)
		}
	}
	assert.Equal(t, []string{SummaryMessageContent("s"), "late"}, conversation)
}
