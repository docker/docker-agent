package agui

import (
	"encoding/json"
	"testing"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

// translateAll renders the AG-UI events produced for a run's runtime events.
func translateAll(t *testing.T, tr *translator, in ...runtime.Event) []string {
	t.Helper()

	var out []events.Event
	for _, event := range in {
		out = append(out, tr.translate(event)...)
	}
	require.NoError(t, events.ValidateSequence(out))

	wire := make([]wireEvent, 0, len(out))
	for _, event := range out {
		data, err := event.ToJSON()
		require.NoError(t, err)
		var decoded wireEvent
		require.NoError(t, json.Unmarshal(data, &decoded))
		wire = append(wire, decoded)
	}
	return render(wire)
}

func partial(id, name, args string) runtime.Event {
	return runtime.PartialToolCall(tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: name, Arguments: args}}, tools.Tool{}, "root")
}

func TestTranslator_MessageBoundariesFollowMessageID(t *testing.T) {
	t.Parallel()

	got := translateAll(t, newTranslator("thread", "run"),
		runtime.StreamStarted("thread", "root"),
		runtime.AgentChoice("root", "thread", "a", "msg-1"),
		runtime.AgentChoice("root", "thread", "", "msg-1"),
		runtime.AgentChoice("root", "thread", "b", "msg-2"),
		// Text of the same message after a tool call needs a new wire message.
		partial("call-1", "lookup", ""),
		runtime.AgentChoice("root", "thread", "c", "msg-2"),
		runtime.StreamStopped("thread", "root", runtime.TurnEndReasonNormal),
		runtime.AgentChoice("root", "thread", "late", "msg-3"),
	)

	assert.Equal(t, []string{
		"RUN_STARTED runId=run threadId=thread",
		"TEXT_MESSAGE_START messageId=m1 role=assistant",
		"TEXT_MESSAGE_CONTENT delta=a messageId=m1",
		"TEXT_MESSAGE_END messageId=m1",
		"TEXT_MESSAGE_START messageId=m2 role=assistant",
		"TEXT_MESSAGE_CONTENT delta=b messageId=m2",
		"TEXT_MESSAGE_END messageId=m2",
		"TOOL_CALL_START toolCallId=call-1 toolCallName=lookup",
		"TEXT_MESSAGE_START messageId=m3 role=assistant",
		"TEXT_MESSAGE_CONTENT delta=c messageId=m3",
		"TEXT_MESSAGE_END messageId=m3",
		"TOOL_CALL_END toolCallId=call-1",
		"RUN_FINISHED runId=run threadId=thread",
	}, got)
}

func TestTranslator_ToolCallVariants(t *testing.T) {
	t.Parallel()

	got := translateAll(t, newTranslator("thread", "run"),
		// A provider that does not stream tool calls.
		runtime.ToolCall(tools.ToolCall{ID: "whole", Function: tools.FunctionCall{Name: "lookup", Arguments: `{"a":1}`}}, tools.Tool{}, "root"),
		runtime.ToolCallResponse("whole", tools.Tool{}, nil, "", "root"),
		// A streamed call the model then retracts.
		partial("retracted", "lookup", "{"),
		runtime.ToolCallsRetracted([]string{"retracted", "unknown"}, "root"),
		partial("retracted", "lookup", "}"),
		// A result without any announced call.
		runtime.ToolCallResponse("orphan", tools.Tool{Name: "lookup"}, nil, "ok", "root"),
		runtime.StreamStopped("thread", "root", runtime.TurnEndReasonNormal),
	)

	assert.Equal(t, []string{
		"RUN_STARTED runId=run threadId=thread",
		"TOOL_CALL_START toolCallId=whole toolCallName=lookup",
		`TOOL_CALL_ARGS delta={"a":1} toolCallId=whole`,
		"TOOL_CALL_END toolCallId=whole",
		"TOOL_CALL_RESULT content=(no output) messageId=whole-result role=tool toolCallId=whole",
		"TOOL_CALL_START toolCallId=retracted toolCallName=lookup",
		"TOOL_CALL_ARGS delta={ toolCallId=retracted",
		"TOOL_CALL_END toolCallId=retracted",
		"TOOL_CALL_START toolCallId=orphan toolCallName=lookup",
		"TOOL_CALL_END toolCallId=orphan",
		"TOOL_CALL_RESULT content=ok messageId=orphan-result role=tool toolCallId=orphan",
		"RUN_FINISHED runId=run threadId=thread",
	}, got)
}

func TestTranslator_SubSessionEventsDoNotEndTheRun(t *testing.T) {
	t.Parallel()

	got := translateAll(t, newTranslator("thread", "run"),
		runtime.StreamStarted("thread", "root"),
		runtime.AgentSwitching(true, "root", "child"),
		runtime.AgentSwitching(true, "root", "child"),
		runtime.StreamStarted("sub", "child"),
		runtime.AgentChoiceReasoning("child", "sub", "thinking", "msg-1"),
		runtime.AgentChoiceReasoningReset("child", "sub", "msg-1"),
		runtime.ErrorForSession("sub", "child failed"),
		runtime.StreamStopped("sub", "child", runtime.TurnEndReasonError),
		// The run ends with the step still open.
		runtime.StreamStopped("thread", "root", runtime.TurnEndReasonNormal),
		runtime.AgentSwitching(false, "child", "root"),
	)

	assert.Equal(t, []string{
		"RUN_STARTED runId=run threadId=thread",
		"STEP_STARTED stepName=child",
		"REASONING_START messageId=m1-reasoning",
		"REASONING_MESSAGE_START messageId=m1-reasoning role=reasoning",
		"REASONING_MESSAGE_CONTENT delta=thinking messageId=m1-reasoning",
		"REASONING_MESSAGE_END messageId=m1-reasoning",
		"REASONING_END messageId=m1-reasoning",
		"STEP_FINISHED stepName=child",
		"RUN_FINISHED runId=run threadId=thread",
	}, got)
}

func TestTranslator_TerminalEvents(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		events []runtime.Event
		end    bool
		cancel bool
		want   []string
	}{
		"root error": {
			events: []runtime.Event{
				runtime.AgentChoice("root", "thread", "partial", "msg-1"),
				runtime.ErrorWithCodeForSession("thread", "rate_limited", "slow down"),
				runtime.StreamStopped("thread", "root", runtime.TurnEndReasonError),
			},
			want: []string{
				"RUN_STARTED runId=run threadId=thread",
				"TEXT_MESSAGE_START messageId=m1 role=assistant",
				"TEXT_MESSAGE_CONTENT delta=partial messageId=m1",
				"RUN_ERROR code=rate_limited message=slow down runId=run",
			},
		},
		"error before the stream started": {
			events: []runtime.Event{runtime.Error("no model")},
			want: []string{
				"RUN_STARTED runId=run threadId=thread",
				"RUN_ERROR code=run_failed message=no model runId=run",
			},
		},
		"stopped on error without an error event": {
			events: []runtime.Event{runtime.StreamStopped("thread", "root", runtime.TurnEndReasonError)},
			want: []string{
				"RUN_STARTED runId=run threadId=thread",
				"RUN_ERROR code=run_failed message=run failed runId=run",
			},
		},
		"canceled": {
			events: []runtime.Event{runtime.StreamStopped("thread", "root", runtime.TurnEndReasonCanceled)},
			want: []string{
				"RUN_STARTED runId=run threadId=thread",
				"RUN_ERROR code=canceled message=run canceled runId=run",
			},
		},
		"stream closed without a stop event": {
			events: []runtime.Event{runtime.AgentChoice("root", "thread", "partial", "msg-1")},
			end:    true,
			want: []string{
				"RUN_STARTED runId=run threadId=thread",
				"TEXT_MESSAGE_START messageId=m1 role=assistant",
				"TEXT_MESSAGE_CONTENT delta=partial messageId=m1",
				"TEXT_MESSAGE_END messageId=m1",
				"RUN_FINISHED runId=run threadId=thread",
			},
		},
		"stream closed by a cancellation": {
			end:    true,
			cancel: true,
			want: []string{
				"RUN_STARTED runId=run threadId=thread",
				"RUN_ERROR code=canceled message=run canceled runId=run",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			tr := newTranslator("thread", "run")
			got := translateAll(t, tr, tc.events...)
			if tc.end {
				require.False(t, tr.terminated())
				for _, event := range tr.end(tc.cancel) {
					data, err := event.ToJSON()
					require.NoError(t, err)
					var decoded wireEvent
					require.NoError(t, json.Unmarshal(data, &decoded))
					got = append(got, render([]wireEvent{decoded})...)
				}
			}
			assert.Equal(t, tc.want, got)
			assert.True(t, tr.terminated())
			assert.Empty(t, tr.end(false))
		})
	}
}

func TestTranslator_CustomEvents(t *testing.T) {
	t.Parallel()

	tr := newTranslator("thread", "run")
	var names []string
	for _, event := range []runtime.Event{
		runtime.Warning("careful", "root"),
		runtime.SessionTitle("thread", "A title"),
		runtime.ModelFallback("root", "a", "b", "overloaded", 1, 3),
	} {
		for _, out := range tr.translate(event) {
			if custom, ok := out.(*events.CustomEvent); ok {
				require.NoError(t, custom.Validate())
				assert.Same(t, event, custom.Value)
				names = append(names, custom.Name)
			}
		}
	}
	assert.Equal(t, []string{"docker-agent.warning", "docker-agent.session_title", "docker-agent.model_fallback"}, names)
}
