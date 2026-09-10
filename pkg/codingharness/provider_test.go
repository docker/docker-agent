package codingharness

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestNewProviderOmitsModelFlagWhenModelEmpty(t *testing.T) {
	t.Parallel()
	p, err := NewProvider(&latest.HarnessConfig{Type: TypeCodex})
	require.NoError(t, err)

	cmd := p.PrintCommand("do it")
	require.Contains(t, cmd, "codex exec --json")
	require.NotContains(t, cmd, " -m ")
}

func TestNewProviderUsesConfiguredModel(t *testing.T) {
	t.Parallel()
	p, err := NewProvider(&latest.HarnessConfig{Type: TypeClaudeCode, Model: "claude-sonnet-4-5", Effort: "high"})
	require.NoError(t, err)

	cmd := p.PrintCommand("do it")
	require.Contains(t, cmd, "--model 'claude-sonnet-4-5'")
	require.Contains(t, cmd, "--effort high")
}
