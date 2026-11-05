//go:build docker_agent_no_hook_templates

package hooks

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModelHookTemplatesDisabled(t *testing.T) {
	t.Parallel()

	_, err := NewModelFactory(nil)(HandlerEnv{}, Hook{Type: HookTypeModel, Model: "openai/gpt-4o", Prompt: "hi"})
	require.ErrorIs(t, err, ErrHookTemplatesDisabled)
}
