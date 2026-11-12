//go:build docker_agent_no_hook_templates

package builtins

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
)

func TestAddContextHookTemplatesDisabled(t *testing.T) {
	t.Parallel()

	_, err := addContext(t.Context(), &hooks.Input{}, []string{"static text"})
	require.ErrorIs(t, err, hooks.ErrHookTemplatesDisabled)
}
