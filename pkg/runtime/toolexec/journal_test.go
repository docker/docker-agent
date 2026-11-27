package toolexec_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// memoryJournal is a [toolexec.Journal] that remembers what it is told, in
// the order it is told.
type memoryJournal struct {
	mu    sync.Mutex
	calls map[string]toolexec.CallRecord
	told  []string
	// failStart makes CallStarted fail, as a store that cannot be written does.
	failStart error
}

func (j *memoryJournal) Call(callID string) toolexec.CallRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.calls[callID]
}

func (j *memoryJournal) CallStarted(_ context.Context, callID string, _ tools.ToolCall) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failStart != nil {
		return j.failStart
	}
	j.told = append(j.told, "started "+callID)
	return nil
}

func (j *memoryJournal) CallRecorded(_ context.Context, callID string, result *tools.ToolCallResult, record func()) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	record()
	j.told = append(j.told, "recorded "+callID+": "+result.Output)
	return nil
}

func (j *memoryJournal) CallFinished(_ context.Context, callID string, outcome toolexec.CallOutcome, record func()) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	told := "finished " + callID
	if record != nil {
		record()
		told += " with its result"
	}
	if outcome.StopRun {
		told += ", stopping the run"
	}
	j.told = append(j.told, told)
	return nil
}

// knowing returns a journal that already keeps a record of call_1.
func knowing(call toolexec.CallRecord) *memoryJournal {
	return &memoryJournal{calls: map[string]toolexec.CallRecord{"call_1": call}}
}

// journaledBatch processes one call of a tool that counts its runs, with a
// journal that may already know the call. delegation says whether the tool
// hands work to another agent; postToolUse, when not nil, answers the tool's
// post_tool_use hook.
func journaledBatch(t *testing.T, journal *memoryJournal, annotations tools.ToolAnnotations, delegation bool, postToolUse *stubHookDispatcher) (runs int, em *captureEmitter, stopRun bool) {
	t.Helper()
	a := newAgent()
	sess := session.New()
	sess.ToolsApproved = true
	tool := tools.Tool{
		Name:        "deploy",
		Annotations: annotations,
		Delegation:  delegation,
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			runs++
			return tools.ResultSuccess("deployed"), nil
		},
	}
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Journal:  journal,
	}
	if postToolUse != nil {
		d.Hooks = postToolUse
	}
	em = &captureEmitter{}
	stopRun, _ = d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "call_1",
		Function: tools.FunctionCall{Name: "deploy", Arguments: `{}`},
	}}, []tools.Tool{tool}, em)
	return runs, em, stopRun
}

// stopAfterTool is a post_tool_use hook that stops the run.
func stopAfterTool() *stubHookDispatcher {
	return &stubHookDispatcher{on: map[hooks.EventType]*hooks.Result{
		hooks.EventPostToolUse: {Allowed: false, Message: "enough"},
	}}
}

func TestJournal_IsToldOfACallStepByStep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		hooks *stubHookDispatcher
		want  []string
	}{
		{
			name: "its result and its outcome go together when nothing follows the result",
			want: []string{"started call_1", "finished call_1 with its result"},
		},
		{
			name:  "its result goes first when a hook follows it",
			hooks: stopAfterTool(),
			want:  []string{"started call_1", "recorded call_1: deployed", "finished call_1, stopping the run"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			journal := &memoryJournal{}

			runs, em, stopRun := journaledBatch(t, journal, tools.ToolAnnotations{}, false, tt.hooks)

			assert.Equal(t, 1, runs)
			assert.Equal(t, tt.hooks != nil, stopRun)
			assert.Equal(t, tt.want, journal.told)
			require.Len(t, em.messages, 1, "the result reaches the session in the step that notes it")
		})
	}
}

func TestJournal_FinishedCallIsLeftAlone(t *testing.T) {
	t.Parallel()
	journal := knowing(toolexec.CallRecord{
		Progress: toolexec.CallFinished,
		Outcome:  toolexec.CallOutcome{StopRun: true, StopMessage: "enough"},
	})

	runs, em, stopRun := journaledBatch(t, journal, tools.ToolAnnotations{}, false, stopAfterTool())

	assert.Zero(t, runs)
	assert.True(t, stopRun, "it means for the run what it meant then")
	assert.Empty(t, em.calls)
	assert.Empty(t, em.messages)
	assert.Empty(t, journal.told)
}

func TestJournal_CallWithItsResultInGetsItsHook(t *testing.T) {
	t.Parallel()
	// The call as a hook had rewritten it, and the whole of what it returned.
	ran := tools.ToolCall{ID: "call_1", Function: tools.FunctionCall{Name: "deploy", Arguments: `{"target":"staging"}`}}
	journal := knowing(toolexec.CallRecord{
		Progress: toolexec.CallRecorded,
		Call:     &ran,
		Result:   tools.ResultSuccess("deployed to staging"),
	})
	hook := stopAfterTool()

	runs, em, stopRun := journaledBatch(t, journal, tools.ToolAnnotations{}, false, hook)

	assert.Zero(t, runs, "the tool had returned")
	assert.Empty(t, em.messages)
	require.NotNil(t, hook.lastPostToolInput)
	assert.Equal(t, "deployed to staging", hook.lastPostToolInput.ToolResponse, "the hook is given what it would have been")
	assert.Equal(t, map[string]any{"target": "staging"}, hook.lastPostToolInput.ToolInput)
	assert.True(t, stopRun)
	assert.Equal(t, []string{"finished call_1, stopping the run"}, journal.told)
}

func TestJournal_CallCutOffMidRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		annotations tools.ToolAnnotations
		delegation  bool
		// ran is the call as the tool was given it before the batch was cut
		// off, when that is not the call the model made.
		ran        *tools.ToolCall
		wantRuns   int
		wantResult string
	}{
		{name: "runs again when its tool is idempotent", annotations: tools.ToolAnnotations{IdempotentHint: true}, wantRuns: 1, wantResult: "deployed"},
		{
			name:        "is reported as interrupted when its tool would be given something else this time",
			annotations: tools.ToolAnnotations{IdempotentHint: true},
			ran:         &tools.ToolCall{ID: "call_1", Function: tools.FunctionCall{Name: "deploy", Arguments: `{"target":"staging"}`}},
			wantResult:  toolexec.InterruptedResult,
		},
		{name: "is reported as interrupted otherwise", wantResult: toolexec.InterruptedResult},
		{
			name:        "is reported as interrupted when its tool is only read-only",
			annotations: tools.ToolAnnotations{ReadOnlyHint: true},
			wantResult:  toolexec.InterruptedResult,
		},
		{
			name:        "is reported as interrupted when its tool hands work to another agent",
			annotations: tools.ToolAnnotations{IdempotentHint: true},
			delegation:  true,
			wantResult:  toolexec.InterruptedResult,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			journal := knowing(toolexec.CallRecord{Progress: toolexec.CallStarted, Call: tt.ran})

			runs, em, stopRun := journaledBatch(t, journal, tt.annotations, tt.delegation, nil)

			assert.Equal(t, tt.wantRuns, runs)
			assert.False(t, stopRun)
			require.Len(t, em.messages, 1)
			assert.Equal(t, tt.wantResult, em.messages[0].Message.Content)
			assert.Equal(t, tt.wantRuns == 0, em.messages[0].Message.IsError)
			assert.Equal(t, "finished call_1 with its result", journal.told[len(journal.told)-1])
		})
	}
}

func TestJournal_ToolItCannotTakeNoteOfDoesNotRun(t *testing.T) {
	t.Parallel()
	journal := &memoryJournal{failStart: errors.New("disk full")}

	runs, _, stopRun := journaledBatch(t, journal, tools.ToolAnnotations{}, false, nil)

	assert.Zero(t, runs)
	assert.True(t, stopRun, "a run that cannot record its progress stops")
}

// cancellingEmitter cancels the run as a call is announced, which is after
// the call was approved and before its tool starts.
type cancellingEmitter struct {
	captureEmitter

	cancel context.CancelFunc
}

func (e *cancellingEmitter) EmitToolCall(tc tools.ToolCall, tool tools.Tool, agentName string) {
	e.cancel()
	e.captureEmitter.EmitToolCall(tc, tool, agentName)
}

func TestJournal_ToolOfARunCancelledBeforeItStartsDoesNotRun(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	journal := &memoryJournal{}
	a := newAgent()
	sess := session.New()
	sess.ToolsApproved = true
	runs := 0
	tool := tools.Tool{
		Name: "deploy",
		// A tool that does not look at its context.
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			runs++
			return tools.ResultSuccess("deployed"), nil
		},
	}
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Journal:  journal,
	}

	d.Process(ctx, sess, []tools.ToolCall{{ID: "call_1", Function: tools.FunctionCall{Name: "deploy", Arguments: `{}`}}}, []tools.Tool{tool}, &cancellingEmitter{cancel: cancel})

	assert.Zero(t, runs, "it would run again, unnoted, when the run is taken up")
	assert.NotContains(t, journal.told, "started call_1")
}
