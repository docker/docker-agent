package teamloader

import (
	"context"
	"strings"

	"github.com/docker/docker-agent/pkg/tools"
)

func WithInstructions(inner tools.ToolSet, instruction string) tools.ToolSet {
	if instruction == "" {
		return inner
	}

	return &replaceInstruction{
		ToolSet:     inner,
		instruction: instruction,
	}
}

type replaceInstruction struct {
	tools.ToolSet
	instruction string
}

// Verify interface compliance
var (
	_ tools.Instructable = (*replaceInstruction)(nil)
	_ tools.Unwrapper    = (*replaceInstruction)(nil)
)

// Unwrap implements tools.Unwrapper.
func (a *replaceInstruction) Unwrap() tools.ToolSet {
	return a.ToolSet
}

// Start forwards the Start call to the inner toolset if it implements Startable.
func (a *replaceInstruction) Start(ctx context.Context) error {
	if startable, ok := a.ToolSet.(tools.Startable); ok {
		return startable.Start(ctx)
	}
	return nil
}

// Stop forwards the Stop call to the inner toolset if it implements Startable.
func (a *replaceInstruction) Stop(ctx context.Context) error {
	if startable, ok := a.ToolSet.(tools.Startable); ok {
		return startable.Stop(ctx)
	}
	return nil
}

func (a *replaceInstruction) Instructions() string {
	original := tools.GetInstructions(a.ToolSet)
	return strings.Replace(a.instruction, "{ORIGINAL_INSTRUCTIONS}", original, 1)
}
