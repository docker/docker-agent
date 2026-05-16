package toolexec

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestResolveModelOverride_NoCalls(t *testing.T) {
	result := ResolveModelOverride(nil, nil)
	assert.Empty(t, result)
}

func TestResolveModelOverride_NoOverride(t *testing.T) {
	agentTools := []tools.Tool{
		{Name: "read_file"},
		{Name: "write_file"},
	}
	calls := []tools.ToolCall{
		{Function: tools.FunctionCall{Name: "read_file"}},
	}

	result := ResolveModelOverride(calls, agentTools)
	assert.Empty(t, result)
}

func TestResolveModelOverride_SingleOverride(t *testing.T) {
	agentTools := []tools.Tool{
		{Name: "read_file", ModelOverride: "openai/gpt-4o-mini"},
		{Name: "write_file"},
	}
	calls := []tools.ToolCall{
		{Function: tools.FunctionCall{Name: "read_file"}},
	}

	result := ResolveModelOverride(calls, agentTools)
	assert.Equal(t, "openai/gpt-4o-mini", result)
}

func TestResolveModelOverride_FirstOverrideWins(t *testing.T) {
	agentTools := []tools.Tool{
		{Name: "read_file", ModelOverride: "openai/gpt-4o-mini"},
		{Name: "search_kb", ModelOverride: "anthropic/claude-haiku"},
	}
	calls := []tools.ToolCall{
		{Function: tools.FunctionCall{Name: "read_file"}},
		{Function: tools.FunctionCall{Name: "search_kb"}},
	}

	result := ResolveModelOverride(calls, agentTools)
	assert.Equal(t, "openai/gpt-4o-mini", result)
}

func TestResolveModelOverride_MixedOverrideAndNonOverride(t *testing.T) {
	agentTools := []tools.Tool{
		{Name: "read_file"},
		{Name: "search_kb", ModelOverride: "openai/gpt-4o-mini"},
	}
	calls := []tools.ToolCall{
		{Function: tools.FunctionCall{Name: "read_file"}},
		{Function: tools.FunctionCall{Name: "search_kb"}},
	}

	// read_file has no override, search_kb does. Since read_file is first
	// but has no override, we skip it and use search_kb's.
	result := ResolveModelOverride(calls, agentTools)
	assert.Equal(t, "openai/gpt-4o-mini", result)
}

func TestResolveModelOverride_UnknownTool(t *testing.T) {
	agentTools := []tools.Tool{
		{Name: "read_file"},
	}
	calls := []tools.ToolCall{
		{Function: tools.FunctionCall{Name: "unknown_tool"}},
	}

	result := ResolveModelOverride(calls, agentTools)
	assert.Empty(t, result)
}
