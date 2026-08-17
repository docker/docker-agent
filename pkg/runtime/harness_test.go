package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	stdruntime "runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// harnessBinDir holds shim executables (codex, claude) shared by every
// harness test. The shims are written exactly once (see TestMain) and
// each cats a per-harness ".out" data file that individual tests
// overwrite. Because the executable bytes never change, the OS
// validates each shim only on first exec instead of paying that cost
// for a brand-new file in every test (~0.25s per first-time exec on
// macOS).
var harnessBinDir string

func TestMain(m *testing.M) {
	// The production JS-command wiring lives in pkg/runtime/jscommands,
	// which in-package tests cannot import (cycle); register the same
	// evaluator directly for the command-resolution tests.
	RegisterCommandEvaluator(func(agentTools []tools.Tool) CommandEvaluator {
		return js.NewEvaluator(agentTools)
	})

	//nolint:forbidigo // TestMain has no *testing.T, so t.TempDir is unavailable.
	dir, err := os.MkdirTemp("", "harness-shim")
	if err != nil {
		panic(err)
	}
	for _, name := range []string{"codex", "claude"} {
		script := fmt.Sprintf("#!/bin/sh\ncat %q\n", filepath.Join(dir, name+".out"))
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			panic(err)
		}
	}
	harnessBinDir = dir

	// The package's user-config reads (cache_stable_prompts, cache_miss_warnings)
	// must see library defaults, not a developer's real ~/.config/cagent/config.yaml.
	// Set once before m.Run and never mutated, so it stays safe for parallel tests.
	//nolint:forbidigo // TestMain has no *testing.T, so t.TempDir is unavailable.
	configDir, err := os.MkdirTemp("", "runtime-test-config-*")
	if err != nil {
		panic(err)
	}
	paths.SetConfigDir(configDir)

	code := m.Run()

	paths.SetConfigDir("")
	_ = os.RemoveAll(configDir)
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// useHarnessShim points PATH at the shared shim directory and sets the
// stdout the named harness program ("codex" or "claude") emits when the
// runtime invokes it. PATH is prepended, not replaced, so the shim can
// still resolve "cat".
func useHarnessShim(t *testing.T, name, out string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(harnessBinDir, name+".out"), []byte(out), 0o600))
	t.Setenv("PATH", harnessBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestHarnessAgentRunStream(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}

	useHarnessShim(t, "codex", `{"type":"item.completed","item":{"type":"agent_message","text":"harness done"}}
`)

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

	useHarnessShim(t, "codex", `{"type":"item.started","item":{"type":"command_execution","command":"npm test"}}
{"type":"item.completed","item":{"type":"agent_message","text":"tests passed"}}
`)

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

	useHarnessShim(t, "claude", `{"type":"assistant","message":{"content":[{"type":"text","text":"I will create the file."},{"type":"tool_use","id":"toolu_write","name":"Write","input":{"file_path":"/tmp/poem.md","content":"roses"}}]}}
{"type":"result","result":"created"}
`)

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

	useHarnessShim(t, "claude", `{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash"}}}
{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"uname -a\"}"}}}
{"type":"stream_event","event":{"type":"content_block_stop","index":1}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"uname -a"}}]}}
{"type":"result","result":"done"}
`)

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

	useHarnessShim(t, "claude", `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}}
{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":" world"}}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Hello world"}]}}
{"type":"result","result":"Hello world"}
`)

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

func newHarnessRuntime(t *testing.T, harnessType string) *LocalRuntime {
	t.Helper()
	root := agent.New("root", "You are an external coder.", agent.WithHarness(&latest.HarnessConfig{Type: harnessType}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
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
