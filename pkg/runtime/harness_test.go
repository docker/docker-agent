package runtime

import (
	"os"
	"path/filepath"
	stdruntime "runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestHarnessAgentRunStream(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	binDir := t.TempDir()
	writeHarnessScript(t, binDir, "codex", `#!/bin/sh
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"harness done"}}'
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	rt := newHarnessRuntime(t, "codex")
	sess := session.New(session.WithUserMessage("do the task"))
	events := collectRuntimeEvents(t, rt, sess)

	assert.True(t, hasEventType(t, events, &AgentChoiceEvent{}))
	assert.Equal(t, "harness done", sess.GetLastAssistantMessageContent())

	var sawHarnessModel bool
	for _, ev := range events {
		if info, ok := ev.(*AgentInfoEvent); ok && info.Model == "codex" {
			sawHarnessModel = true
		}
	}
	assert.True(t, sawHarnessModel, "expected AgentInfo event with codex harness label")
}

func TestHarnessToolCallCompletes(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	binDir := t.TempDir()
	writeHarnessScript(t, binDir, "codex", `#!/bin/sh
printf '%s\n' '{"type":"item.started","item":{"type":"command_execution","command":"npm test"}}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"tests passed"}}'
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	rt := newHarnessRuntime(t, "codex")
	events := collectRuntimeEvents(t, rt, session.New(session.WithUserMessage("run tests")))

	var toolCall *ToolCallEvent
	var toolResponse *ToolCallResponseEvent
	for _, ev := range events {
		switch ev := ev.(type) {
		case *ToolCallEvent:
			toolCall = ev
		case *ToolCallResponseEvent:
			toolResponse = ev
		}
	}
	require.NotNil(t, toolCall)
	require.NotNil(t, toolResponse)
	assert.Equal(t, toolCall.ToolCall.ID, toolResponse.ToolCallID)
	require.NotNil(t, toolResponse.Result)
	assert.False(t, toolResponse.Result.IsError)
}

func TestHarnessShowsClaudeCodeToolCallAlongsideText(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	binDir := t.TempDir()
	writeHarnessScript(t, binDir, "claude", `#!/bin/sh
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"I will create the file."},{"type":"tool_use","id":"toolu_write","name":"Write","input":{"file_path":"/tmp/poem.md","content":"roses"}}]}}'
printf '%s\n' '{"type":"result","result":"created"}'
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	rt := newHarnessRuntime(t, "claude-code")
	events := collectRuntimeEvents(t, rt, session.New(session.WithUserMessage("write poem")))

	var sawText bool
	var toolCall *ToolCallEvent
	for _, ev := range events {
		switch ev := ev.(type) {
		case *AgentChoiceEvent:
			if strings.Contains(ev.Content, "I will create the file") {
				sawText = true
			}
		case *ToolCallEvent:
			toolCall = ev
		}
	}
	assert.True(t, sawText)
	require.NotNil(t, toolCall)
	assert.Equal(t, "Write", toolCall.ToolCall.Function.Name)
	assert.Contains(t, toolCall.ToolCall.Function.Arguments, "/tmp/poem.md")
}

func TestHarnessSuppressesDuplicateClaudeCodeToolCall(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	binDir := t.TempDir()
	writeHarnessScript(t, binDir, "claude", `#!/bin/sh
printf '%s\n' '{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash"}}}'
printf '%s\n' '{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"uname -a\"}"}}}'
printf '%s\n' '{"type":"stream_event","event":{"type":"content_block_stop","index":1}}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"uname -a"}}]}}'
printf '%s\n' '{"type":"result","result":"done"}'
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	rt := newHarnessRuntime(t, "claude-code")
	events := collectRuntimeEvents(t, rt, session.New(session.WithUserMessage("run uname")))

	var toolCalls []ToolCallEvent
	var partialArgs strings.Builder
	for _, ev := range events {
		switch ev := ev.(type) {
		case *ToolCallEvent:
			toolCalls = append(toolCalls, *ev)
		case *PartialToolCallEvent:
			partialArgs.WriteString(ev.ToolCall.Function.Arguments)
		}
	}
	require.Len(t, toolCalls, 1)
	assert.Equal(t, "Bash", toolCalls[0].ToolCall.Function.Name)
	assert.Contains(t, partialArgs.String(), "uname -a")
}

func TestHarnessSuppressesReplayedClaudeCodeFinalText(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	binDir := t.TempDir()
	writeHarnessScript(t, binDir, "claude", `#!/bin/sh
printf '%s\n' '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}}'
printf '%s\n' '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":" world"}}}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"Hello world"}]}}'
printf '%s\n' '{"type":"result","result":"Hello world"}'
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	rt := newHarnessRuntime(t, "claude-code")
	events := collectRuntimeEvents(t, rt, session.New(session.WithUserMessage("say hello")))

	var chunks []string
	for _, ev := range events {
		if choice, ok := ev.(*AgentChoiceEvent); ok {
			chunks = append(chunks, choice.Content)
		}
	}
	assert.Equal(t, []string{"Hello", " world"}, chunks)
}

// TestHarnessAgentRefusesPlanMode pins the plan-mode-vs-harness invariant:
// the runtime owns the toolset in the normal LLM loop and can strip
// non-read-only tools, but a harness-backed agent delegates the whole
// turn (tools included) to an external library. Rather than degrade
// plan mode to "advisory prompt only" — which the reminder text
// explicitly contradicts — the runtime refuses the turn and surfaces
// an unsupported_mode error so the user can switch back to build mode
// or pick a non-harness agent.
func TestHarnessAgentRefusesPlanMode(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	binDir := t.TempDir()
	// Intentionally produces output that would normally be surfaced as
	// an assistant message; the test asserts that the harness never
	// runs, so this output should be dropped.
	writeHarnessScript(t, binDir, "codex", `#!/bin/sh
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"this should not appear"}}'
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	rt := newHarnessRuntime(t, "codex")
	sess := session.New(
		session.WithUserMessage("do the task"),
		session.WithMode(session.ModePlan),
	)
	events := collectRuntimeEvents(t, rt, sess)

	var errEvent *ErrorEvent
	for _, ev := range events {
		if e, ok := ev.(*ErrorEvent); ok {
			errEvent = e
			break
		}
	}
	require.NotNil(t, errEvent, "expected ErrorEvent rejecting plan mode for harness agent")
	assert.Equal(t, ErrorCodeUnsupportedMode, errEvent.Code)
	assert.Contains(t, errEvent.Error, "plan mode")
	// Harness must not have produced any assistant content.
	assert.Empty(t, sess.GetLastAssistantMessageContent())
}

func writeHarnessScript(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755))
}

func newHarnessRuntime(t *testing.T, harnessType string) *LocalRuntime {
	t.Helper()
	root := agent.New("root", "You are an external coder.", agent.WithHarness(&latest.HarnessConfig{Type: harnessType}))
	rt, err := NewLocalRuntime(team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	return rt
}

func collectRuntimeEvents(t *testing.T, rt *LocalRuntime, sess *session.Session) []Event {
	t.Helper()
	var events []Event
	for ev := range rt.RunStream(t.Context(), sess) {
		events = append(events, ev)
	}
	return events
}
