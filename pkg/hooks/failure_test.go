package hooks

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHookFailurePolicyMatrix(t *testing.T) {
	t.Parallel()

	for _, event := range []EventType{EventPreToolUse, EventToolGuard, EventToolInputTransform, EventBeforeLLMCall} {
		for _, policy := range []string{"", "warn", "ignore", "block"} {
			for _, tc := range []struct {
				name   string
				result HandlerResult
				err    error
			}{
				{name: "execution", err: errors.New("failure")},
				{name: "timeout", err: context.DeadlineExceeded},
				{name: "exit 1", result: HandlerResult{ExitCode: 1}},
				{name: "missing dependency", result: HandlerResult{ExitCode: 127}},
				{name: "malformed JSON", result: HandlerResult{Stdout: `{"decision":`}},
				{name: "bad decision", result: HandlerResult{Stdout: `{"decision":"blok"}`}},
				{name: "bad permission", result: HandlerResult{Stdout: `{"hook_specific_output":{"permission_decision":"alow"}}`}},
			} {
				t.Run(string(event)+"/"+policy+"/"+tc.name, func(t *testing.T) {
					t.Parallel()
					exec := pipelineTestExecutor(t, event, []Hook{{Type: "fail", Command: "test", OnError: policy}}, failingHandlerRegistry(tc.result, tc.err))
					result, err := exec.Dispatch(t.Context(), event, &Input{})
					require.NoError(t, err)
					blocked := EventContract(event).FailClosed || policy == "block"
					assert.Equal(t, !blocked, result.Allowed)
					switch {
					case blocked:
						assert.Equal(t, -1, result.ExitCode)
						assert.Contains(t, result.Message, string(event)+" hook failed")
						assert.Contains(t, result.Message, `hook "test"`)
					case policy == "ignore":
						assert.Empty(t, result.SystemMessage)
					default:
						assert.Contains(t, result.SystemMessage, "hook failed")
					}
				})
			}
		}
	}
}

func TestStrictHookOutputProtocol(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, stdout string
		strict, fail bool
	}{
		{name: "plain compatibility", stdout: "context"},
		{name: "strict plain", stdout: "context", strict: true, fail: true},
		{name: "empty", strict: true},
		{name: "JSON", stdout: `{}`, strict: true},
		{name: "unknown field", stdout: `{"unknown_field":false}`, strict: true, fail: true},
		{name: "unknown nested field", stdout: `{"hook_specific_output":{"updated_inpu":{}}}`, strict: true, fail: true},
		{name: "multiple objects", stdout: `{} {}`, fail: true},
		{name: "JSON log suffix", stdout: `{} done`, fail: true},
		{name: "array", stdout: `[]`, strict: true, fail: true},
		{name: "null", stdout: `null`, strict: true, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseStdoutJSON(tc.stdout, tc.strict)
			if tc.fail {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestStrictHookRejectsWrongEventAndUnsupportedOutput(t *testing.T) {
	t.Parallel()
	for _, stdout := range []string{
		`{"hook_specific_output":{"hook_event_name":"stop"}}`,
		`{"hook_specific_output":{"updated_input":{"cmd":"rewrite"}}}`,
	} {
		exec := pipelineTestExecutor(t, EventToolGuard, []Hook{{Type: "fail", Command: "guard", StrictOutput: true}}, failingHandlerRegistry(HandlerResult{Stdout: stdout}, nil))
		result, err := exec.Dispatch(t.Context(), EventToolGuard, &Input{})
		require.NoError(t, err)
		assert.False(t, result.Allowed)
		assert.Nil(t, result.ModifiedInput)
	}
}

func TestUnsupportedBlockOutputsWarn(t *testing.T) {
	t.Parallel()
	for _, res := range []HandlerResult{
		{ExitCode: 2},
		{Stdout: `{"decision":"block"}`},
		{Stdout: `{"continue":false}`},
		{Stdout: `{"hook_specific_output":{"permission_decision":"deny"}}`},
	} {
		exec := NewExecutorWithRegistry(&Config{Stop: []Hook{{Type: "fail", Command: "stop"}}}, "", nil, failingHandlerRegistry(res, nil))
		result, err := exec.Dispatch(t.Context(), EventStop, &Input{})
		require.NoError(t, err)
		assert.True(t, result.Allowed)
		assert.NotEmpty(t, result.SystemMessage)
	}
}

func TestOutputEventNameIsCompatibilityMetadata(t *testing.T) {
	t.Parallel()
	out := &Output{HookSpecificOutput: &HookSpecificOutput{HookEventName: EventTurnStart, AdditionalContext: "context"}}
	require.NoError(t, validateOutput(EventSessionStart, out, false))
	require.Error(t, validateOutput(EventSessionStart, out, true))
}

func TestInvalidDispatchInput(t *testing.T) {
	t.Parallel()
	exec := NewExecutor(&Config{PreToolUse: []MatcherConfig{{Hooks: trueHook}}}, "", nil)
	_, err := exec.Dispatch(t.Context(), EventPreToolUse, nil)
	require.ErrorContains(t, err, "input must not be nil")
	_, err = exec.Dispatch(t.Context(), EventType("unknown"), &Input{})
	require.ErrorContains(t, err, "unknown hook event")
}
