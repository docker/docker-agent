package teamloader

import (
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

func (a *replaceInstruction) Instructions() string {
	original := tools.GetInstructions(a.ToolSet)
	return strings.Replace(a.instruction, "{ORIGINAL_INSTRUCTIONS}", original, 1)
}
