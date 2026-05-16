package teamloader

import (
	"context"

	"github.com/docker/docker-agent/pkg/tools"
)

// WithModelOverride wraps a toolset so that every tool it produces carries the
// given model in its ModelOverride field, enabling per-toolset model routing.
func WithModelOverride(inner tools.ToolSet, model string) tools.ToolSet {
	if model == "" {
		return inner
	}

	return &modelOverrideToolset{
		ToolSet: inner,
		model:   model,
	}
}

type modelOverrideToolset struct {
	tools.ToolSet

	model string
}

var (
	_ tools.Instructable = (*modelOverrideToolset)(nil)
	_ tools.Unwrapper    = (*modelOverrideToolset)(nil)
)

func (m *modelOverrideToolset) Unwrap() tools.ToolSet {
	return m.ToolSet
}

func (m *modelOverrideToolset) Instructions() string {
	return tools.GetInstructions(m.ToolSet)
}

func (m *modelOverrideToolset) Tools(ctx context.Context) ([]tools.Tool, error) {
	innerTools, err := m.ToolSet.Tools(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]tools.Tool, len(innerTools))
	for i, t := range innerTools {
		t.ModelOverride = m.model
		result[i] = t
	}

	return result, nil
}
