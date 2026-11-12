package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
)

func TestInstructionContextKeepsInitialPromptStableAndAppendsChanges(t *testing.T) {
	sess := New()
	a := agent.New("test", "base prompt")
	sess.AddMessage(UserMessage("first"))

	require.True(t, sess.PrepareInstructionContext([]InstructionSource{{
		Key: "core/date", Label: "date", Content: "Today's date: 2026-07-15", Available: true,
	}}))
	first := sess.GetMessages(a)
	require.Len(t, first, 3)
	assert.Equal(t, chat.MessageRoleSystem, first[1].Role)
	assert.Equal(t, "Today's date: 2026-07-15", first[1].Content)

	assert.False(t, sess.PrepareInstructionContext([]InstructionSource{{
		Key: "core/date", Label: "date", Content: "Today's date: 2026-07-15", Available: true,
	}}))
	assert.Equal(t, first, sess.GetMessages(a))

	sess.AddMessage(NewAgentMessage("test", &chat.Message{Role: chat.MessageRoleAssistant, Content: "reply"}))
	require.True(t, sess.PrepareInstructionContext([]InstructionSource{{
		Key: "core/date", Label: "date", Content: "Today's date: 2026-07-16", Available: true,
	}}))
	updated := sess.GetMessages(a)

	assert.Equal(t, first[0].Content, updated[0].Content)
	assert.Equal(t, first[1].Content, updated[1].Content, "the initial snapshot must remain byte-stable")
	require.Len(t, updated, 5)
	assert.Equal(t, chat.MessageRoleUser, updated[4].Role)
	assert.Contains(t, updated[4].Content, "<system-update>")
	assert.Contains(t, updated[4].Content, "2026-07-16")
}

func TestInstructionContextUsesSourceSpecificChangeNarration(t *testing.T) {
	sess := New()
	a := agent.New("test", "base prompt")
	sess.PrepareInstructionContext([]InstructionSource{{
		Key: "core/date", Content: "Today's date: 2026-07-15", Available: true,
	}})
	sess.AddMessage(UserMessage("hello"))
	sess.PrepareInstructionContext([]InstructionSource{{
		Key:            "core/date",
		Content:        "Today's date: 2026-07-16",
		ChangedContent: "Today's date is now: 2026-07-16",
		Available:      true,
	}})

	messages := sess.GetMessages(a)
	require.Len(t, messages, 4)
	assert.Contains(t, messages[3].Content, "Today's date is now: 2026-07-16")
	assert.NotContains(t, messages[3].Content, "turn-start")
	assert.NotContains(t, messages[3].Content, "context under")
}

func TestInstructionContextUpdatesOnlyChangedGroupMember(t *testing.T) {
	sess := New()
	a := agent.New("test", "base prompt")
	initial := []InstructionSource{
		{Key: "core/prompt-file-a", Group: "core/prompt-files", Content: "Instructions from: /a\na1", RemovedContent: "removed /a", CompleteGroup: true, Available: true},
		{Key: "core/prompt-file-b", Group: "core/prompt-files", Content: "Instructions from: /b\nb1", RemovedContent: "removed /b", Available: true},
	}
	sess.PrepareInstructionContext(initial)
	sess.AddMessage(UserMessage("hello"))
	sess.PrepareInstructionContext([]InstructionSource{
		{Key: "core/prompt-file-a", Group: "core/prompt-files", Content: "Instructions from: /a\na2", ChangedContent: "changed /a", RemovedContent: "removed /a", CompleteGroup: true, Available: true},
		{Key: "core/prompt-file-b", Group: "core/prompt-files", Content: "Instructions from: /b\nb1", RemovedContent: "removed /b", Available: true},
	})

	messages := sess.GetMessages(a)
	update := messages[len(messages)-1].Content
	assert.Contains(t, update, "changed /a")
	assert.NotContains(t, update, "Instructions from: /b")

	sess.AddMessage(NewAgentMessage("test", &chat.Message{Role: chat.MessageRoleAssistant, Content: "reply"}))
	sess.PrepareInstructionContext([]InstructionSource{
		{Key: "core/prompt-file-a", Group: "core/prompt-files", Content: "Instructions from: /a\na2", ChangedContent: "changed /a", RemovedContent: "removed /a", CompleteGroup: true, Available: true},
	})
	messages = sess.GetMessages(a)
	assert.Contains(t, messages[len(messages)-1].Content, "removed /b")
	assert.NotContains(t, messages[len(messages)-1].Content, "Instructions from: /a")
}

func TestInstructionContextAdvancesEpochAfterCompaction(t *testing.T) {
	sess := New()
	a := agent.New("test", "base prompt")
	sess.AddMessage(UserMessage("first"))
	sess.PrepareInstructionContext([]InstructionSource{{Key: "core/value", Content: "old", Available: true}})
	sess.AddMessage(NewAgentMessage("test", &chat.Message{Role: chat.MessageRoleAssistant, Content: "reply"}))
	sess.PrepareInstructionContext([]InstructionSource{{Key: "core/value", Content: "new", Available: true}})

	sess.ApplyCompaction(0, 0, Item{Summary: "summary"})
	require.True(t, sess.PrepareInstructionContext([]InstructionSource{{Key: "core/value", Content: "new", Available: true}}))
	messages := sess.GetMessages(a)

	require.GreaterOrEqual(t, len(messages), 3)
	assert.Equal(t, "new", messages[1].Content)
	for _, message := range messages {
		assert.NotContains(t, message.Content, "<system-update>")
	}
}

func TestInstructionContextUnavailableReadKeepsCurrentValue(t *testing.T) {
	sess := New()
	sess.PrepareInstructionContext([]InstructionSource{{Key: "core/value", Content: "known", Available: true}})

	assert.False(t, sess.PrepareInstructionContext([]InstructionSource{{Key: "core/value", Available: false}}))
	assert.Equal(t, "known", sess.InstructionContext.Current["core/value"].Content)
	assert.Empty(t, sess.InstructionContext.Updates)
}

func TestGetMessagesWithoutInstructionContextUsesLegacyExtras(t *testing.T) {
	sess := New()
	a := agent.New("test", "base prompt")
	sess.PrepareInstructionContext([]InstructionSource{{Key: "core/date", Content: "old date", Available: true}})
	sess.AddMessage(UserMessage("hello"))

	messages := sess.GetMessagesWithoutInstructionContext(a, chat.Message{
		Role: chat.MessageRoleSystem, Content: "current date",
	})
	require.Len(t, messages, 3)
	assert.Equal(t, "base prompt", messages[0].Content)
	assert.Equal(t, "current date", messages[1].Content)
	assert.Equal(t, "hello", messages[2].Content)
}

func TestInstructionContextPersistsInSQLite(t *testing.T) {
	store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	sess := New(WithID("instruction-context"))
	sess.PrepareInstructionContext([]InstructionSource{{Key: "core/value", Content: "old", Available: true}})
	sess.AddMessage(UserMessage("hello"))
	sess.PrepareInstructionContext([]InstructionSource{{Key: "core/value", Content: "new", Available: true}})
	require.NoError(t, store.AddSession(t.Context(), sess))

	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.NotNil(t, loaded.InstructionContext)
	assert.Equal(t, "old", loaded.InstructionContext.Initial["core/value"].Content)
	assert.Equal(t, "new", loaded.InstructionContext.Current["core/value"].Content)
	require.Len(t, loaded.InstructionContext.Updates, 1)
	assert.Equal(t, 1, loaded.InstructionContext.Updates[0].Position)
}

func TestInstructionContextSnapshotPreservesCheckedBytes(t *testing.T) {
	t.Parallel()
	sess := New(WithUserMessage("hello"))
	source := InstructionSource{Key: "file", Path: "/AGENTS.md", Group: "core/prompt-files", Content: "approved", Available: true}
	sess.PrepareInstructionContext([]InstructionSource{source})
	snapshot := sess.InstructionContextSnapshot()
	require.NotNil(t, snapshot)
	assert.Equal(t, "/AGENTS.md", snapshot.Initial["file"].Path)
	source.Content = "replacement"
	sess.PrepareInstructionContext([]InstructionSource{source})
	assert.Empty(t, snapshot.Updates)
	assert.Equal(t, "approved", snapshot.Current["file"].Content)
	messages, count := sess.GetMessagesWithInstructionContext(agent.New("test", "system"), snapshot)
	assert.Equal(t, sess.ItemCount(), count)
	for _, message := range messages {
		assert.NotContains(t, message.Content, "replacement")
	}
	assert.Equal(t, "approved", messages[1].Content)
	delete(snapshot.Current, "file")
	assert.Contains(t, sess.InstructionContextSnapshot().Current, "file")
}
