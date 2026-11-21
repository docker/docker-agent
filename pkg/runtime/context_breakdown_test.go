package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// newBreakdownRuntime builds a LocalRuntime around a single agent with a
// mock model whose context window is 1000 tokens.
func newBreakdownRuntime(t *testing.T, agentOpts ...agent.Opt) *LocalRuntime {
	t.Helper()

	opts := append([]agent.Opt{
		agent.WithModel(&mockProvider{id: "test/mock-model"}),
	}, agentOpts...)
	root := agent.New("root", "You are a test agent", opts...)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStoreWithLimit{limit: 1000}),
	)
	require.NoError(t, err)
	return rt
}

func TestContextBreakdown_CategorizesMessages(t *testing.T) {
	t.Parallel()

	promptDir := t.TempDir()
	promptFile := "CONTEXT_BREAKDOWN_TEST_PROMPT.md"
	promptContent := "Always be excellent to each other."
	require.NoError(t, os.WriteFile(filepath.Join(promptDir, promptFile), []byte(promptContent), 0o600))

	rt := newBreakdownRuntime(t,
		agent.WithTools(
			tools.Tool{Name: "echo", Description: "Echoes input", Parameters: map[string]any{"type": "object"}},
			tools.Tool{Name: "list", Description: "Lists files"},
		),
		agent.WithAddPromptFiles([]string{promptFile}),
	)
	rt.workingDir = promptDir

	sess := session.New(session.WithUserMessage("hello"))
	sess.AddMessage(&session.Message{Message: chat.Message{
		Role:      chat.MessageRoleAssistant,
		Content:   "let me check",
		ToolCalls: []tools.ToolCall{{ID: "call_1", Function: tools.FunctionCall{Name: "echo", Arguments: `{"text":"hi"}`}}},
	}})
	sess.AddMessage(&session.Message{Message: chat.Message{
		Role:       chat.MessageRoleTool,
		ToolCallID: "call_1",
		Content:    "hi",
	}})

	b, err := rt.ContextBreakdown(t.Context(), sess)
	require.NoError(t, err)

	assert.Equal(t, "test/mock-model", b.Model)
	assert.Equal(t, int64(1000), b.ContextLimit)

	// One invariant system message: the agent instruction.
	assert.Equal(t, 1, b.SystemPrompt.Items)
	assert.Positive(t, b.SystemPrompt.Tokens)

	// Two static tools.
	assert.Equal(t, 2, b.ToolDefinitions.Items)
	assert.Positive(t, b.ToolDefinitions.Tokens)

	// One prompt file found in the working directory.
	assert.Equal(t, 1, b.PromptFiles.Items)
	expectedPromptTokens := compaction.EstimateMessageTokens(&chat.Message{
		Role:    chat.MessageRoleSystem,
		Content: promptContent,
	})
	assert.Equal(t, expectedPromptTokens, b.PromptFiles.Tokens)

	// user + assistant turns; the tool result is its own category.
	assert.Equal(t, 2, b.Messages.Items)
	assert.Positive(t, b.Messages.Tokens)
	assert.Equal(t, 1, b.ToolResults.Items)
	assert.Positive(t, b.ToolResults.Tokens)

	// No compaction happened.
	assert.Zero(t, b.CompactionSummary.Items)
	assert.Zero(t, b.CompactionSummary.Tokens)
	assert.Empty(t, b.LatestCompactionSummary)

	total := b.SystemPrompt.Tokens + b.ToolDefinitions.Tokens + b.PromptFiles.Tokens +
		b.Messages.Tokens + b.ToolResults.Tokens + b.CompactionSummary.Tokens
	assert.Equal(t, total, b.TotalTokens())
}

func TestContextBreakdown_CompactionSummary(t *testing.T) {
	t.Parallel()

	rt := newBreakdownRuntime(t)

	items := []session.Item{
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "old question"}}),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "old answer"}}),
		{Summary: "We discussed old things."},
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "new question"}}),
	}
	sess := session.New(session.WithMessages(items))

	b, err := rt.ContextBreakdown(t.Context(), sess)
	require.NoError(t, err)

	// The synthetic summary message lands in its own category; the
	// pre-compaction turns are gone and only the new question remains.
	assert.Equal(t, 1, b.CompactionSummary.Items)
	assert.Positive(t, b.CompactionSummary.Tokens)
	assert.Equal(t, 1, b.Messages.Items)

	// The verbatim summary text is exposed for display.
	assert.Equal(t, "We discussed old things.", b.LatestCompactionSummary)
}

// TestContextBreakdown_LatestCompactionSummaryWins verifies that after
// multiple compactions the exposed text is the most recent summary.
func TestContextBreakdown_LatestCompactionSummaryWins(t *testing.T) {
	t.Parallel()

	rt := newBreakdownRuntime(t)

	items := []session.Item{
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "first question"}}),
		{Summary: "First summary."},
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "second question"}}),
		{Summary: "Second summary."},
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "third question"}}),
	}
	sess := session.New(session.WithMessages(items))

	b, err := rt.ContextBreakdown(t.Context(), sess)
	require.NoError(t, err)

	assert.Equal(t, "Second summary.", b.LatestCompactionSummary)
}

// TestContextBreakdown_UserMessageLooksLikeSummary pins the exact-match
// classification: a user message that merely starts with the summary prefix
// must stay in the conversation bucket.
func TestContextBreakdown_UserMessageLooksLikeSummary(t *testing.T) {
	t.Parallel()

	rt := newBreakdownRuntime(t)
	sess := session.New(session.WithUserMessage("Session Summary: not actually one"))

	b, err := rt.ContextBreakdown(t.Context(), sess)
	require.NoError(t, err)

	assert.Zero(t, b.CompactionSummary.Items)
	assert.Equal(t, 1, b.Messages.Items)
}

func TestContextBreakdown_ReportedUsageWins(t *testing.T) {
	t.Parallel()

	rt := newBreakdownRuntime(t)

	sess := session.New(session.WithUserMessage("hi"))
	sess.AddMessage(&session.Message{Message: chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "hello there",
		Usage:   &chat.Usage{InputTokens: 10, OutputTokens: 42},
	}})

	b, err := rt.ContextBreakdown(t.Context(), sess)
	require.NoError(t, err)

	// The assistant turn carries provider-reported usage, so its estimate is
	// the reported output count (plus the per-message overhead), not the
	// chars-per-token heuristic. The user turn stays heuristic.
	userTokens := compaction.EstimateMessageTokens(&chat.Message{Role: chat.MessageRoleUser, Content: "hi"})
	assistantTokens := compaction.EstimateMessageTokens(&chat.Message{
		Role: chat.MessageRoleAssistant, Content: "hello there",
		Usage: &chat.Usage{InputTokens: 10, OutputTokens: 42},
	})
	assert.Equal(t, userTokens+assistantTokens, b.Messages.Tokens)
	assert.Equal(t, 2, b.Messages.Items)
}

func TestContextBreakdown_CompactionModelCapsLimit(t *testing.T) {
	t.Parallel()

	store := mapModelStore{limits: map[string]int{"primary/big": 200_000, "compaction/small": 16_000}}
	primary := &mockProvider{id: "primary/big"}
	compactionModel := &mockProvider{id: "compaction/small"}
	root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(compactionModel))
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(store))
	require.NoError(t, err)

	b, err := rt.ContextBreakdown(t.Context(), session.New(session.WithUserMessage("hi")))
	require.NoError(t, err)

	assert.Equal(t, int64(16_000), b.ContextLimit, "the effective limit is capped by the smaller compaction model")
	assert.Equal(t, "compaction/small", b.CompactionModel)
	assert.Equal(t, int64(16_000), b.CompactionContextLimit)
	assert.Equal(t, int64(200_000), b.PrimaryContextLimit)
}

func TestContextBreakdown_CompactionModelEqualOrLargerFieldsPresentButNotCapping(t *testing.T) {
	t.Parallel()

	store := mapModelStore{limits: map[string]int{"primary/big": 200_000, "compaction/big": 200_000}}
	primary := &mockProvider{id: "primary/big"}
	compactionModel := &mockProvider{id: "compaction/big"}
	root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(compactionModel))
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(store))
	require.NoError(t, err)

	b, err := rt.ContextBreakdown(t.Context(), session.New(session.WithUserMessage("hi")))
	require.NoError(t, err)

	assert.Equal(t, int64(200_000), b.ContextLimit, "a same-or-larger compaction model never caps the limit")
	// The identity/window fields are still populated — renderers decide
	// whether to attribute a cap by comparing CompactionContextLimit against
	// PrimaryContextLimit, not by the field's mere presence.
	assert.Equal(t, "compaction/big", b.CompactionModel)
	assert.Equal(t, int64(200_000), b.CompactionContextLimit)
	assert.Equal(t, int64(200_000), b.PrimaryContextLimit)
}

func TestContextBreakdown_NoCompactionModelLeavesAttributionZero(t *testing.T) {
	t.Parallel()

	rt := newBreakdownRuntime(t)

	b, err := rt.ContextBreakdown(t.Context(), session.New(session.WithUserMessage("hi")))
	require.NoError(t, err)

	assert.Empty(t, b.CompactionModel)
	assert.Zero(t, b.CompactionContextLimit)
	assert.Zero(t, b.PrimaryContextLimit)
}

func TestContextBreakdown_UnresolvableCompactionWindowLeavesAttributionZero(t *testing.T) {
	t.Parallel()

	// The compaction model has no catalogue entry and no
	// context_window, so its window can't be resolved: the effective limit
	// must fall back to the primary. The model identity is still reported
	// (a dedicated model IS configured), but its window is 0.
	store := mapModelStore{limits: map[string]int{"primary/big": 200_000}}
	primary := &mockProvider{id: "primary/big"}
	compactionModel := &mockProvider{id: "compaction/unresolvable"}
	root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(compactionModel))
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(store))
	require.NoError(t, err)

	b, err := rt.ContextBreakdown(t.Context(), session.New(session.WithUserMessage("hi")))
	require.NoError(t, err)

	assert.Equal(t, int64(200_000), b.ContextLimit)
	assert.Equal(t, "compaction/unresolvable", b.CompactionModel)
	assert.Zero(t, b.CompactionContextLimit)
	assert.Equal(t, int64(200_000), b.PrimaryContextLimit)
}

func TestContextBreakdown_UnknownContextLimit(t *testing.T) {
	t.Parallel()

	root := agent.New("root", "You are a test agent",
		agent.WithModel(&mockProvider{id: "test/mock-model"}))
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}), // no catalogue entry
	)
	require.NoError(t, err)

	b, err := rt.ContextBreakdown(t.Context(), session.New(session.WithUserMessage("hi")))
	require.NoError(t, err)
	assert.Zero(t, b.ContextLimit)
	assert.Positive(t, b.TotalTokens())
}

func TestContextBreakdown_NilSession(t *testing.T) {
	t.Parallel()

	rt := newBreakdownRuntime(t)
	_, err := rt.ContextBreakdown(t.Context(), nil)
	require.Error(t, err)
}

func TestContextBreakdown_NoPromptFiles(t *testing.T) {
	t.Parallel()

	rt := newBreakdownRuntime(t)
	rt.workingDir = t.TempDir()

	b, err := rt.ContextBreakdown(t.Context(), session.New(session.WithUserMessage("hi")))
	require.NoError(t, err)
	assert.Zero(t, b.PromptFiles.Items)
	assert.Zero(t, b.PromptFiles.Tokens)
	assert.Empty(t, b.PromptFileItems)
}

func TestContextBreakdown_PromptFileItems(t *testing.T) {
	t.Parallel()

	promptDir := t.TempDir()
	promptFile := "PROMPT_ITEMS_TEST.md"
	promptContent := "Be concise. Prefer tables over prose when reporting results."
	require.NoError(t, os.WriteFile(filepath.Join(promptDir, promptFile), []byte(promptContent), 0o600))

	rt := newBreakdownRuntime(t, agent.WithAddPromptFiles([]string{promptFile}))
	rt.workingDir = promptDir

	b, err := rt.ContextBreakdown(t.Context(), session.New(session.WithUserMessage("hi")))
	require.NoError(t, err)

	require.Len(t, b.PromptFileItems, 1)
	item := b.PromptFileItems[0]
	assert.Equal(t, filepath.Join(promptDir, promptFile), item.Path)
	assert.False(t, item.Missing)
	expected := compaction.EstimateMessageTokens(&chat.Message{
		Role:    chat.MessageRoleSystem,
		Content: promptContent,
	})
	assert.Equal(t, expected, item.Tokens)
}

func TestContextBreakdown_AttachedFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	textPath := filepath.Join(dir, "notes.md")
	textContent := "Attached notes: the retry loop lives in pkg/runtime and backs off exponentially."
	require.NoError(t, os.WriteFile(textPath, []byte(textContent), 0o600))
	missingPath := filepath.Join(dir, "deleted.txt")

	rt := newBreakdownRuntime(t)
	sess := session.New(session.WithUserMessage("hello"))
	sess.AddAttachedFile(textPath)
	sess.AddAttachedFile(missingPath)

	b, err := rt.ContextBreakdown(t.Context(), sess)
	require.NoError(t, err)

	require.Len(t, b.AttachedFiles, 2)

	text := b.AttachedFiles[0]
	assert.Equal(t, textPath, text.Path)
	assert.False(t, text.Missing)
	expected := compaction.EstimateMessageTokens(&chat.Message{
		Role:    chat.MessageRoleUser,
		Content: textContent,
	})
	assert.Equal(t, expected, text.Tokens)

	missing := b.AttachedFiles[1]
	assert.Equal(t, missingPath, missing.Path)
	assert.True(t, missing.Missing)
	assert.Zero(t, missing.Tokens)

	// Attached-file estimates detail content already counted in Messages;
	// they must not inflate the estimated prompt total.
	assert.Equal(t,
		b.SystemPrompt.Tokens+b.ToolDefinitions.Tokens+b.PromptFiles.Tokens+
			b.Messages.Tokens+b.ToolResults.Tokens+b.CompactionSummary.Tokens,
		b.TotalTokens())
}

func TestContextBreakdown_NoAttachedFiles(t *testing.T) {
	t.Parallel()

	rt := newBreakdownRuntime(t)
	b, err := rt.ContextBreakdown(t.Context(), session.New(session.WithUserMessage("hi")))
	require.NoError(t, err)
	assert.Empty(t, b.AttachedFiles)
}

func TestAttachedFileTokens_Kinds(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	estimator := compaction.Estimator{}

	// A minimal valid PNG header makes content sniffing classify the file
	// as a supported binary type, which is sized as a flat attachment charge.
	pngPath := filepath.Join(dir, "shot.png")
	pngHeader := append([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, make([]byte, 64)...)
	require.NoError(t, os.WriteFile(pngPath, pngHeader, 0o600))
	binaryTokens := attachedFileTokens(t.Context(), estimator, pngPath, int64(len(pngHeader)))
	expectedBinary := compaction.EstimateMessageTokens(&chat.Message{
		Role: chat.MessageRoleUser,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Source: chat.DocumentSource{InlineData: []byte{0}}}},
		},
	})
	assert.Equal(t, expectedBinary, binaryTokens)

	// Text files above the inline cap were never inlined, so they
	// contribute nothing.
	textPath := filepath.Join(dir, "big.txt")
	require.NoError(t, os.WriteFile(textPath, []byte("content"), 0o600))
	assert.Zero(t, attachedFileTokens(t.Context(), estimator, textPath, chat.MaxInlineFileSize+1))
}

func TestEstimateToolDefinitionTokens(t *testing.T) {
	t.Parallel()

	withSchema := estimateToolDefinitionTokens(&tools.Tool{
		Name:        "shell",
		Description: "Run a shell command",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}},
	})
	withoutSchema := estimateToolDefinitionTokens(&tools.Tool{Name: "noop"})

	assert.Positive(t, withoutSchema)
	assert.Greater(t, withSchema, withoutSchema, "the parameters schema must contribute to the estimate")
}
