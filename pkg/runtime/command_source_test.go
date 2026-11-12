package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/tools"
)

type lookupOnlySource struct {
	calls int
}

func (s *lookupOnlySource) CurrentAgentInfo(context.Context) CurrentAgentInfo {
	s.calls++
	return CurrentAgentInfo{Commands: types.Commands{"test": {Instruction: "instruction"}}}
}

func TestCommandLookupNeedsOnlyAgentInfo(t *testing.T) {
	t.Parallel()
	source := &lookupOnlySource{}
	_, _, ok := LookupCommand(t.Context(), source, "ordinary message")
	assert.False(t, ok)
	assert.Zero(t, source.calls)
	command, rest, ok := LookupCommand(t.Context(), source, "/test arguments")
	assert.True(t, ok)
	assert.Equal(t, "instruction", command.Instruction)
	assert.Equal(t, "arguments", rest)
	assert.Equal(t, 1, source.calls)
}

type recordingCommandSource struct {
	commands types.Commands
	calls    []string
}

func (s *recordingCommandSource) CurrentAgentInfo(context.Context) CurrentAgentInfo {
	s.calls = append(s.calls, "info")
	return CurrentAgentInfo{Commands: s.commands}
}

func (s *recordingCommandSource) CurrentAgentTools(context.Context) ([]tools.Tool, error) {
	s.calls = append(s.calls, "tools")
	return []tools.Tool{{Name: "echo", Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
		s.calls = append(s.calls, "handler")
		return tools.ResultSuccess("${untrusted}"), nil
	}}}, nil
}

func (s *recordingCommandSource) CommandEvaluatorFactory() CommandEvaluatorFactory {
	s.calls = append(s.calls, "factory")
	return func([]tools.Tool) CommandEvaluator {
		return commandEvaluatorFunc(func(_ context.Context, instruction string, _ []string) string {
			s.calls = append(s.calls, "evaluate")
			return instruction
		})
	}
}

func TestCommandSourcePreservesLazyDiscovery(t *testing.T) {
	t.Parallel()
	source := &recordingCommandSource{commands: types.Commands{
		"agent": {Agent: "worker"},
		"test":  {Instruction: "!echo()"},
	}}
	assert.Equal(t, "message", ResolveCommand(t.Context(), source, "message"))
	assert.Empty(t, source.calls)
	assert.Equal(t, "/unknown", ResolveCommand(t.Context(), source, "/unknown"))
	assert.Equal(t, []string{"info"}, source.calls)
	source.calls = nil
	assert.Equal(t, "task", ResolveCommand(t.Context(), source, "/agent task"))
	assert.Equal(t, []string{"info"}, source.calls)
	source.calls = nil
	assert.Equal(t, "${untrusted}", ResolveCommand(t.Context(), source, "/test"))
	assert.Equal(t, []string{"info", "factory", "tools", "evaluate", "tools", "handler"}, source.calls)
}

type toolsOnlySource struct {
	calls int
}

func (s *toolsOnlySource) CurrentAgentTools(context.Context) ([]tools.Tool, error) {
	s.calls++
	return nil, nil
}

func TestToolCommandsNeedOnlyTools(t *testing.T) {
	t.Parallel()
	source := &toolsOnlySource{}
	assert.Equal(t, "ordinary text", executeToolCommands(t.Context(), source, "ordinary text"))
	assert.Zero(t, source.calls)
	assert.Contains(t, executeToolCommands(t.Context(), source, "!missing()"), "not found")
	assert.Equal(t, 1, source.calls)
}
