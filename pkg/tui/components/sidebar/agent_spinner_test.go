package sidebar

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	handoff "github.com/docker/docker-agent/pkg/tools/builtin/handoff/types"
	transfertask "github.com/docker/docker-agent/pkg/tools/builtin/transfertask/types"
)

func TestAgentSpinnerFollowsHandoffAndTransfer(t *testing.T) {
	t.Parallel()

	roster := transferRoster()
	m := newAgentPanelSidebar(t, 40, roster...)
	t.Cleanup(m.spinner.Stop)

	handoffTool := tools.Tool{Name: handoff.ToolNameHandoff}
	transferTool := tools.Tool{Name: transfertask.ToolNameTransferTask}
	toolCall := func(tool tools.Tool, agent string) runtime.Event {
		return runtime.ToolCall(tools.ToolCall{ID: tool.Name, Function: tools.FunctionCall{Name: tool.Name}}, tool, agent)
	}
	toolResponse := func(tool tools.Tool, agent string) runtime.Event {
		return runtime.ToolCallResponse(tool.Name, tool, tools.ResultSuccess("done"), "done", agent)
	}

	turn := func(sessionID, agent string) runtime.Event {
		return &runtime.AgentInfoEvent{SessionID: sessionID, AgentName: agent, AgentContext: runtime.AgentContext{AgentName: agent}}
	}

	steps := []struct {
		event runtime.Event
		owner string
	}{
		{runtime.AgentInfo("root", "anthropic/opus", "", ""), ""},
		{runtime.StreamStarted("root-session", "root"), "root"},
		{runtime.AgentInfo("root", "anthropic/opus", "", ""), "root"},
		// The handoff tool result is still from root; the next turn's metadata
		// is the first event that moves the spinner, without a new stream.
		{toolCall(handoffTool, "root"), "root"},
		{toolResponse(handoffTool, "root"), "root"},
		{runtime.ToolsetInfo(1, false, "Scout"), "root"},
		{runtime.AgentInfo("Scout", "openai/gpt-5.4-mini", "", ""), "root"},
		{turn("root-session", "Scout"), "Scout"},
		// A transfer announces Coder before starting its separate stream.
		{toolCall(transferTool, "Scout"), "Scout"},
		{runtime.AgentSwitching(true, "Scout", "Coder"), "Scout"},
		{runtime.AgentInfo("Coder", "anthropic/claude-sonnet-4-6", "", ""), "Scout"},
		{runtime.TeamInfo(roster, "Coder"), "Scout"},
		{runtime.ToolsetInfo(1, false, "Coder"), "Scout"},
		{runtime.StreamStarted("child-session", "Coder"), "Coder"},
		{turn("child-session", "Coder"), "Coder"},
		// Only the parent's tool result restarts the spinner after the child stops.
		{runtime.StreamStopped("child-session", "Coder", ""), ""},
		{runtime.SubSessionCompleted("root-session", session.New(), "Scout"), ""},
		{runtime.AgentSwitching(false, "Coder", "Scout"), ""},
		{runtime.AgentInfo("Scout", "openai/gpt-5.4-mini", "", ""), ""},
		{toolResponse(transferTool, "Scout"), "Scout"},
		{runtime.StreamStopped("root-session", "Scout", ""), ""},
		{runtime.AgentInfo("Coder", "anthropic/claude-sonnet-4-6", "", ""), ""},
	}
	for i, step := range steps {
		// The TUI records event attribution before forwarding to the sidebar.
		m.sessionState.SetCurrentAgentName(step.event.GetAgentName())
		_, _ = m.Update(step.event)
		require.Equalf(t, step.owner != "", m.spinnerActive, "step %d: %T", i, step.event)
		for _, agent := range roster {
			line, _ := agentLines(m, agent.Name)
			assert.Equalf(t, agent.Name == step.owner, strings.HasPrefix(line, m.spinner.RawFrame()),
				"step %d: %T, %s roster line %q", i, step.event, agent.Name, line)
		}
	}
}
