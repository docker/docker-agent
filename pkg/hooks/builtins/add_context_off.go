//go:build docker_agent_no_hook_templates

package builtins

import (
	"context"
	"fmt"

	"github.com/docker/docker-agent/pkg/hooks"
)

// AddContext is the registered name of the add_context builtin.
const AddContext = "add_context"

func addContext(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
	return nil, fmt.Errorf("add_context: %w", hooks.ErrHookTemplatesDisabled)
}
