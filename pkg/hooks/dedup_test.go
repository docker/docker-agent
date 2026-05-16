package hooks

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecutorDedupsByTypeCommandArgs pins that two structurally identical
// hook entries collapse to one invocation, while two builtin hooks with
// the SAME name but DIFFERENT Args remain distinct and both fire.
//
// This is the contract the runtime relies on when WithAddPromptFiles
// auto-injects a hook AND a user explicitly authors another
// add_prompt_files entry with a different file list: both must run.
func TestExecutorDedupsByTypeCommandArgs(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	registry := NewRegistry()
	require.NoError(t, registry.RegisterBuiltin("count", func(_ context.Context, _ *Input, _ []string) (*Output, error) {
		calls.Add(1)
		return nil, nil
	}))

	cfg := &Config{
		SessionStart: []Hook{
			// The next two are structurally identical and must collapse.
			{Type: HookTypeBuiltin, Command: "count", Args: []string{"a"}},
			{Type: HookTypeBuiltin, Command: "count", Args: []string{"a"}},
			// Same name, different Args -> distinct invocation.
			{Type: HookTypeBuiltin, Command: "count", Args: []string{"b"}},
			// No-args version -> also distinct from both above.
			{Type: HookTypeBuiltin, Command: "count"},
		},
	}

	exec := NewExecutorWithRegistry(cfg, t.TempDir(), nil, registry)
	_, err := exec.Dispatch(t.Context(), EventSessionStart, &Input{SessionID: "s"})
	require.NoError(t, err)

	// Three distinct (command, args) tuples -> three invocations.
	assert.Equal(t, int32(3), calls.Load())
}
