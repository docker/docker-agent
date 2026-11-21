package transcript

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	handoff "github.com/docker/docker-agent/pkg/tools/builtin/handoff/types"
	transfertask "github.com/docker/docker-agent/pkg/tools/builtin/transfertask/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

const testAgent = "Gordon"

func newTestTranscript() *Transcript {
	return New(animation.NewRuntime(), service.StaticSessionState{AgentName: testAgent})
}

func toolCall(id string) (tools.ToolCall, tools.Tool) {
	call := tools.ToolCall{
		ID:       id,
		Function: tools.FunctionCall{Name: "shell", Arguments: "{}"},
	}
	return call, tools.Tool{Name: "shell"}
}

func TestAppendAndRender(t *testing.T) {
	t.Parallel()
	tr := newTestTranscript()

	_ = tr.Append(types.User("hello"))
	_ = tr.Append(types.Agent(types.MessageTypeAssistant, testAgent, "hi there"))

	out := tr.Render(80)
	assert.Contains(t, out, "hello")
	assert.Contains(t, out, "hi there")
	assert.NotContains(t, ansi.Strip(out), testAgent)
	// One blank separator line between the two messages.
	assert.Contains(t, out, "\n\n")
}

func TestAppendToLastMessageStreamsIntoSameMessage(t *testing.T) {
	t.Parallel()
	tr := newTestTranscript()

	_ = tr.AppendToLastMessage(testAgent, "first")
	_ = tr.AppendToLastMessage(testAgent, " second")

	require.True(t, tr.LastIs(types.MessageTypeAssistant))
	require.Len(t, tr.msgs, 1)
	assert.Equal(t, "first second", tr.msgs[0].Content)
	assert.Contains(t, tr.Render(80), "first second")
}

func TestAppendToLastMessageReplacesSpinner(t *testing.T) {
	t.Parallel()
	tr := newTestTranscript()
	defer tr.StopAnimations()

	_ = tr.Append(types.Spinner())
	_ = tr.AppendToLastMessage(testAgent, "reply")

	require.Len(t, tr.msgs, 1)
	assert.Equal(t, types.MessageTypeAssistant, tr.msgs[0].Type)
}

func TestAppendToLastMessageNewMessagePerSender(t *testing.T) {
	t.Parallel()
	tr := newTestTranscript()

	_ = tr.AppendToLastMessage("agent-a", "from a")
	_ = tr.AppendToLastMessage("agent-b", "from b")

	require.Len(t, tr.msgs, 2)
	assert.Equal(t, "from a", tr.msgs[0].Content)
	assert.Equal(t, "from b", tr.msgs[1].Content)
}

func TestAgentAttribution(t *testing.T) {
	t.Parallel()

	tr := newTestTranscript()
	t.Cleanup(tr.StopAnimations)
	_ = tr.Append(types.User("Find the facts."))
	_ = tr.Append(types.Agent(types.MessageTypeAssistant, "root", "Here are the facts."))
	_ = tr.AppendToLastMessage("reviewer", " ")
	assert.NotContains(t, ansi.Strip(tr.Render(80)), "reviewer", "whitespace is not a reply")
	_ = tr.AppendToLastMessage("reviewer", "Reviewing")
	_ = tr.AppendToLastMessage("reviewer", " the facts.")
	_ = tr.Append(types.AgentReturn("reviewer", "root"))
	_ = tr.Append(types.Agent(types.MessageTypeAssistant, "root", "Ready to proceed."))
	_ = tr.Append(types.User("Start over."))
	_ = tr.AppendToLastMessage("reviewer", "Starting over.")

	out := ansi.Strip(tr.Render(80))
	assert.Regexp(t, `(?s)Here are the facts\..*root\s*→\s*reviewer.*Reviewing the facts\..*reviewer\s+returned control to\s+root.*Ready to proceed\..*Starting over\.`, out)
	assert.Equal(t, 1, strings.Count(out, "→"))
	assert.Equal(t, 2, strings.Count(out, "reviewer"))
	assert.Equal(t, 2, strings.Count(out, "root"))
	require.Len(t, tr.Messages(), 7, "inferred cues are render-only")
	_ = tr.Rebuild()
	assert.Equal(t, out, ansi.Strip(tr.Render(80)))
}

func TestToolTransitionAttribution(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, tool, next, row, inferred string
		status                          types.ToolStatus
		finalize                        bool
	}{
		{"explicit handoff", handoff.ToolNameHandoff, "reviewer", `root\s*─►\s*reviewer`, "", types.ToolStatusCompleted, false},
		{"failed handoff", handoff.ToolNameHandoff, "summarizer", `✗\s+root\s+handoff to\s+reviewer`, `root\s*→\s*summarizer`, types.ToolStatusError, false},
		{"failed transfer", transfertask.ToolNameTransferTask, "root", `root\s+calls\s+reviewer`, "", types.ToolStatusError, false},
		{"silent transfer", transfertask.ToolNameTransferTask, "root", `root\s+calls\s+reviewer`, "", types.ToolStatusCompleted, false},
		{"interrupted transfer with reply", transfertask.ToolNameTransferTask, "reviewer", `root\s+calls\s+reviewer`, "", types.ToolStatusRunning, true},
		{"interrupted transfer after a silent switch", transfertask.ToolNameTransferTask, "summarizer", `root\s+calls\s+reviewer`, `reviewer\s*→\s*summarizer`, types.ToolStatusRunning, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTestTranscript()
			t.Cleanup(tr.StopAnimations)
			_ = tr.AppendToLastMessage("root", "Here are the facts.")
			call := tools.ToolCall{ID: "transition", Function: tools.FunctionCall{Name: tc.tool, Arguments: `{"agent":"reviewer","task":"Check the facts."}`}}
			_ = tr.AddOrUpdateToolCall("root", call, tools.Tool{Name: tc.tool}, tc.status)
			_ = tr.AppendToLastMessage(tc.next, "Continuing the work.")
			if tc.finalize {
				tr.FinalizeToolCalls(types.ToolStatusError)
				assert.False(t, tr.ar.HasActive())
			}
			out := ansi.Strip(tr.Render(80))
			assert.Regexp(t, `(?s)Here are the facts\..*`+tc.row+`.*`+tc.inferred+`.*Continuing the work\.`, out)
			if tc.inferred == "" {
				assert.NotContains(t, out, "→")
			} else {
				assert.Equal(t, 1, strings.Count(out, "→"))
			}
			if tc.tool == transfertask.ToolNameTransferTask {
				assert.Equal(t, 1, strings.Count(out, "calls"))
			}
			_ = tr.Rebuild()
			assert.Equal(t, out, ansi.Strip(tr.Render(80)))
		})
	}
}

func TestAddOrUpdateToolCall(t *testing.T) {
	t.Parallel()
	tr := newTestTranscript()
	defer tr.StopAnimations()

	call, def := toolCall("call-1")
	_ = tr.AddOrUpdateToolCall(testAgent, call, def, types.ToolStatusRunning)
	require.Len(t, tr.msgs, 1)
	assert.Equal(t, types.ToolStatusRunning, tr.msgs[0].ToolStatus)

	// Same ID updates in place instead of duplicating.
	_ = tr.AddOrUpdateToolCall(testAgent, call, def, types.ToolStatusCompleted)
	require.Len(t, tr.msgs, 1)
	assert.Equal(t, types.ToolStatusCompleted, tr.msgs[0].ToolStatus)

	// A new ID appends a new entry.
	call2, def2 := toolCall("call-2")
	_ = tr.AddOrUpdateToolCall(testAgent, call2, def2, types.ToolStatusRunning)
	require.Len(t, tr.msgs, 2)
}

func TestSetToolStatus(t *testing.T) {
	t.Parallel()
	tr := newTestTranscript()
	defer tr.StopAnimations()

	call, def := toolCall("call-1")
	_ = tr.Append(types.ToolCallMessage(testAgent, call, def, types.ToolStatusConfirmation))

	_, ok := tr.SetToolStatus("call-1", types.ToolStatusRunning)
	require.True(t, ok)
	assert.Equal(t, types.ToolStatusRunning, tr.msgs[0].ToolStatus)

	_, ok = tr.SetToolStatus("no-such-call", types.ToolStatusError)
	assert.False(t, ok)
}

func TestFinalizeToolCalls(t *testing.T) {
	t.Parallel()
	tr := newTestTranscript()
	defer tr.StopAnimations()

	running, runningDef := toolCall("call-1")
	done, doneDef := toolCall("call-2")
	_ = tr.Append(types.ToolCallMessage(testAgent, running, runningDef, types.ToolStatusRunning))
	_ = tr.Append(types.ToolCallMessage(testAgent, done, doneDef, types.ToolStatusCompleted))

	tr.FinalizeToolCalls(types.ToolStatusError)

	assert.Equal(t, types.ToolStatusError, tr.msgs[0].ToolStatus)
	// Already-terminal entries are left alone.
	assert.Equal(t, types.ToolStatusCompleted, tr.msgs[1].ToolStatus)
}

func TestConsecutiveToolCallsGroupTightly(t *testing.T) {
	t.Parallel()
	tr := newTestTranscript()
	defer tr.StopAnimations()

	call1, def1 := toolCall("call-1")
	call2, def2 := toolCall("call-2")
	_ = tr.Append(types.ToolCallMessage(testAgent, call1, def1, types.ToolStatusCompleted))
	_ = tr.Append(types.ToolCallMessage(testAgent, call2, def2, types.ToolStatusCompleted))

	for line := range strings.SplitSeq(tr.Render(80), "\n") {
		assert.NotEmpty(t, strings.TrimSpace(line), "consecutive tool calls must not be separated by a blank line")
	}
}

func TestRemoveLast(t *testing.T) {
	t.Parallel()
	tr := newTestTranscript()
	defer tr.StopAnimations()

	_ = tr.Append(types.User("hello"))
	_ = tr.Append(types.Spinner())
	require.True(t, tr.LastIs(types.MessageTypeSpinner))

	tr.RemoveLast(types.MessageTypeSpinner)
	assert.True(t, tr.LastIs(types.MessageTypeUser))

	// Wrong type: no-op.
	tr.RemoveLast(types.MessageTypeSpinner)
	assert.True(t, tr.LastIs(types.MessageTypeUser))
}

func TestRebuildPreservesContent(t *testing.T) {
	t.Parallel()
	tr := newTestTranscript()
	defer tr.StopAnimations()

	_ = tr.Append(types.User("hello"))
	call, def := toolCall("call-1")
	_ = tr.Append(types.ToolCallMessage(testAgent, call, def, types.ToolStatusCompleted))
	before := tr.Render(80)

	_ = tr.Rebuild()

	assert.Equal(t, before, tr.Render(80))
}

func TestRenderReflowsOnWidthChange(t *testing.T) {
	t.Parallel()
	tr := newTestTranscript()

	_ = tr.Append(types.Agent(types.MessageTypeAssistant, testAgent,
		strings.Repeat("word ", 40)))

	wide := tr.Render(120)
	narrow := tr.Render(40)
	assert.NotEqual(t, wide, narrow)
	for line := range strings.SplitSeq(narrow, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 40)
	}
}

func TestAddOrUpdateToolCallArgumentsAndStartedAt(t *testing.T) {
	t.Parallel()
	tr := newTestTranscript()
	defer tr.StopAnimations()

	// Streamed argument deltas accumulate while pending.
	call, def := toolCall("call-1")
	call.Function.Arguments = `{"cmd":`
	_ = tr.AddOrUpdateToolCall(testAgent, call, def, types.ToolStatusPending)
	call.Function.Arguments = `"ls"}`
	_ = tr.AddOrUpdateToolCall(testAgent, call, def, types.ToolStatusPending)
	require.Len(t, tr.msgs, 1)
	assert.JSONEq(t, `{"cmd":"ls"}`, tr.msgs[0].ToolCall.Function.Arguments)
	assert.Nil(t, tr.msgs[0].StartedAt)

	// The running transition carries the full arguments and a start time.
	call.Function.Arguments = `{"cmd":"ls -la"}`
	_ = tr.AddOrUpdateToolCall(testAgent, call, def, types.ToolStatusRunning)
	assert.JSONEq(t, `{"cmd":"ls -la"}`, tr.msgs[0].ToolCall.Function.Arguments)
	assert.NotNil(t, tr.msgs[0].StartedAt)
}

func TestSetToolStatusRunningSetsStartedAt(t *testing.T) {
	t.Parallel()
	tr := newTestTranscript()
	defer tr.StopAnimations()

	call, def := toolCall("call-1")
	_ = tr.Append(types.ToolCallMessage(testAgent, call, def, types.ToolStatusConfirmation))
	require.Nil(t, tr.msgs[0].StartedAt)

	_, ok := tr.SetToolStatus("call-1", types.ToolStatusRunning)
	require.True(t, ok)
	assert.NotNil(t, tr.msgs[0].StartedAt)
	assert.Nil(t, tr.msgs[0].FinishedAt)

	_, ok = tr.SetToolStatus("call-1", types.ToolStatusCompleted)
	require.True(t, ok)
	require.NotNil(t, tr.msgs[0].FinishedAt)
	assert.Contains(t, ansi.Strip(tr.Render(80)), "Completed in 0s")
	finished := tr.msgs[0].FinishedAt
	_, ok = tr.SetToolStatus("call-1", types.ToolStatusCompleted)
	require.True(t, ok)
	assert.Same(t, finished, tr.msgs[0].FinishedAt)
}

func TestTransferTaskGetsSeparator(t *testing.T) {
	t.Parallel()
	tr := newTestTranscript()
	defer tr.StopAnimations()

	call1, def1 := toolCall("call-1")
	_ = tr.Append(types.ToolCallMessage(testAgent, call1, def1, types.ToolStatusCompleted))
	transfer := tools.ToolCall{
		ID:       "call-2",
		Function: tools.FunctionCall{Name: transfertask.ToolNameTransferTask, Arguments: "{}"},
	}
	_ = tr.Append(types.ToolCallMessage(testAgent, transfer, tools.Tool{Name: transfertask.ToolNameTransferTask}, types.ToolStatusCompleted))

	// Unlike other consecutive tool calls, a handoff stands apart.
	assert.Contains(t, tr.Render(80), "\n\n")
}

func TestMessages(t *testing.T) {
	t.Parallel()
	tr := newTestTranscript()

	assert.Empty(t, tr.Messages())

	_ = tr.Append(types.User("hello"))
	_ = tr.AppendToLastMessage(testAgent, "hi")

	msgs := tr.Messages()
	require.Len(t, msgs, 2)
	assert.Equal(t, types.MessageTypeUser, msgs[0].Type)
	assert.Equal(t, "hi", msgs[1].Content)
}
