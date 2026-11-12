package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks"
)

func TestInstructionObservationLegacyMessagesIncludesSemanticContext(t *testing.T) {
	observation := observeInstructions(&hooks.Result{
		AdditionalContext: "custom context",
		InstructionContext: []hooks.InstructionContext{
			{Key: "core/date", Content: "Today's date: 2026-07-15"},
			{Key: "core/prompt-file-a", Content: "Instructions from: /a\ncontent"},
			{Group: "core/prompt-files", SetMarker: true},
			{Key: "core/unavailable", Content: "stale", Unavailable: true},
		},
	}, false)

	messages := observation.legacyMessages()
	require.Len(t, messages, 3)
	assert.Equal(t, []string{
		"custom context",
		"Today's date: 2026-07-15",
		"Instructions from: /a\ncontent",
	}, []string{messages[0].Content, messages[1].Content, messages[2].Content})
	for _, message := range messages {
		assert.Equal(t, chat.MessageRoleSystem, message.Role)
	}
}

func TestInstructionObservationLoaderFailureRequiresConfiguredGuard(t *testing.T) {
	t.Parallel()
	failure := &hooks.Result{FailedHooks: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: "add_prompt_files"}}}
	assert.Empty(t, observeInstructions(failure, false).sources)
	guarded := observeInstructions(failure, true)
	require.Len(t, guarded.sources, 1)
	assert.False(t, guarded.sources[0].Available)
	assert.True(t, guarded.sources[0].SetMarker)
	assert.Equal(t, "core/prompt-files", guarded.sources[0].Group)
}
