package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestPipelineToolInputPatchesCompose(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	require.NoError(t, registry.RegisterBuiltin("append", func(_ context.Context, in *Input, args []string) (*Output, error) {
		assert.Equal(t, "work", in.ToolInput["cwd"])
		return &Output{HookSpecificOutput: &HookSpecificOutput{
			UpdatedInput: map[string]any{"cmd": in.ToolInput["cmd"].(string) + args[0]},
		}}, nil
	}))
	exec := NewExecutorWithRegistry(&Config{PreToolUse: []MatcherConfig{
		{Matcher: "other", Hooks: []Hook{{Type: HookTypeCommand, Command: "exit 2"}}},
		{Matcher: "shell", Hooks: []Hook{
			{Type: HookTypeCommand, Command: `echo '{"hook_specific_output":{"updated_input":{"cmd":"ls"}}}'`},
			{Type: HookTypeBuiltin, Command: "append", Args: []string{" -l"}},
		}},
		{Matcher: "*", Hooks: []Hook{{Type: HookTypeBuiltin, Command: "append", Args: []string{" -h"}}}},
	}}, t.TempDir(), nil, registry)
	original := map[string]any{"cmd": "original", "cwd": "work", "nested": map[string]any{"keep": true}}
	in := &Input{ToolName: "shell", ToolInput: original}

	result, err := exec.Dispatch(t.Context(), EventPreToolUse, in)
	require.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Equal(t, map[string]any{"cmd": "ls -l -h", "cwd": "work", "nested": map[string]any{"keep": true}}, result.ModifiedInput)
	assert.Equal(t, "original", original["cmd"])
	assert.Equal(t, original, in.ToolInput)
}

func TestPipelineMessagesCompose(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	require.NoError(t, registry.RegisterBuiltin("append", func(_ context.Context, in *Input, args []string) (*Output, error) {
		in.Messages[0].Content += args[0]
		return &Output{HookSpecificOutput: &HookSpecificOutput{UpdatedMessages: in.Messages}}, nil
	}))
	require.NoError(t, registry.RegisterBuiltin("empty", func(_ context.Context, in *Input, _ []string) (*Output, error) {
		assert.Equal(t, "original-a-b", in.Messages[0].Content)
		return &Output{HookSpecificOutput: &HookSpecificOutput{UpdatedMessages: []chat.Message{}}}, nil
	}))
	exec := NewExecutorWithRegistry(&Config{BeforeLLMCall: []Hook{
		{Type: HookTypeBuiltin, Command: "append", Args: []string{"-a"}},
		{Type: HookTypeCommand, Command: "echo '{}'"},
		{Type: HookTypeBuiltin, Command: "append", Args: []string{"-b"}},
		{Type: HookTypeBuiltin, Command: "empty"},
	}}, t.TempDir(), nil, registry)
	in := &Input{Messages: []chat.Message{{Role: chat.MessageRoleUser, Content: "original"}}}

	result, err := exec.Dispatch(t.Context(), EventBeforeLLMCall, in)
	require.NoError(t, err)
	require.Len(t, result.UpdatedMessages, 1)
	assert.Equal(t, "original-a-b", result.UpdatedMessages[0].Content)
	assert.Equal(t, "original", in.Messages[0].Content)
}

func TestPipelineToolResponseComposesAndPreservesEmptyRewrite(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		clearResponse bool
		want          string
	}{
		{name: "append", want: "first-second"},
		{name: "clear", clearResponse: true, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			registry := NewRegistry()
			require.NoError(t, registry.RegisterBuiltin("rewrite", func(_ context.Context, in *Input, _ []string) (*Output, error) {
				assert.Equal(t, "first", in.ToolResponse)
				updated := in.ToolResponse.(string) + "-second"
				if tc.clearResponse {
					updated = ""
				}
				return &Output{HookSpecificOutput: &HookSpecificOutput{UpdatedToolResponse: &updated}}, nil
			}))
			require.NoError(t, registry.RegisterBuiltin("observe", func(_ context.Context, in *Input, _ []string) (*Output, error) {
				assert.Equal(t, tc.want, in.ToolResponse)
				return nil, nil
			}))
			exec := NewExecutorWithRegistry(&Config{ToolResponseTransform: []MatcherConfig{{Hooks: []Hook{
				{Type: HookTypeCommand, Command: `echo '{"hook_specific_output":{"updated_tool_response":"first"}}'`},
				{Type: HookTypeBuiltin, Command: "rewrite"},
				{Type: HookTypeBuiltin, Command: "observe"},
			}}}}, t.TempDir(), nil, registry)
			in := &Input{ToolName: "shell", ToolResponse: "original"}

			result, err := exec.Dispatch(t.Context(), EventToolResponseTransform, in)
			require.NoError(t, err)
			require.NotNil(t, result.UpdatedToolResponse)
			assert.Equal(t, tc.want, *result.UpdatedToolResponse)
			assert.Equal(t, "original", in.ToolResponse)
		})
	}
}

func TestPipelineToolInputEmptyPatch(t *testing.T) {
	t.Parallel()

	for _, input := range []map[string]any{nil, {"cmd": "original", "cwd": "work"}} {
		exec := NewExecutor(&Config{PreToolUse: []MatcherConfig{{Hooks: []Hook{
			{Type: HookTypeCommand, Command: `echo '{"hook_specific_output":{"updated_input":{}}}'`},
		}}}}, t.TempDir(), nil)
		result, err := exec.Dispatch(t.Context(), EventPreToolUse, &Input{ToolInput: input})
		require.NoError(t, err)
		require.NotNil(t, result.ModifiedInput)
		assert.Len(t, result.ModifiedInput, len(input))
		for key, value := range input {
			assert.Equal(t, value, result.ModifiedInput[key])
		}
	}
}

func TestPipelineNoRewrite(t *testing.T) {
	t.Parallel()

	for _, event := range []EventType{EventPreToolUse, EventBeforeLLMCall, EventToolResponseTransform, EventToolInputTransform} {
		t.Run(string(event), func(t *testing.T) {
			t.Parallel()
			exec := pipelineTestExecutor(t, event, []Hook{{Type: HookTypeCommand, Command: "echo '{}'"}}, NewRegistry())
			result, err := exec.Dispatch(t.Context(), event, &Input{ToolInput: map[string]any{"cmd": "original"}})
			require.NoError(t, err)
			assert.Nil(t, result.ModifiedInput)
			assert.Nil(t, result.UpdatedMessages)
			assert.Nil(t, result.UpdatedToolResponse)
		})
	}
}

func TestPipelineFailuresKeepPriorRewriteAndRunRemainingHooks(t *testing.T) {
	t.Parallel()

	for _, event := range []EventType{EventPreToolUse, EventBeforeLLMCall, EventToolResponseTransform, EventToolInputTransform} {
		for _, tc := range []struct {
			name    string
			result  HandlerResult
			err     error
			onError string
			blocked bool
		}{
			{name: "execution error", err: errors.New("failed"), onError: "block", blocked: true},
			{name: "canceled", err: context.Canceled, onError: "block", blocked: true},
			{name: "nonblocking exit", result: HandlerResult{ExitCode: 1}},
			{name: "blocking exit", result: HandlerResult{ExitCode: 2}, blocked: true},
		} {
			t.Run(string(event)+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				registry := NewRegistry()
				lastRan := false
				registry.Register("test", func(_ HandlerEnv, hook Hook) (Handler, error) {
					return pipelineHandlerFunc(func(_ context.Context, data []byte) (HandlerResult, error) {
						var in Input
						if err := json.Unmarshal(data, &in); err != nil {
							return HandlerResult{}, err
						}
						if hook.Command == "last" {
							lastRan = true
							assert.Equal(t, "first", in.ToolInput["cmd"])
							assert.Equal(t, "first", in.Messages[0].Content)
							assert.Equal(t, "first", in.ToolResponse)
							return HandlerResult{}, nil
						}
						res := tc.result
						response := "discarded"
						res.Output = &Output{HookSpecificOutput: &HookSpecificOutput{
							UpdatedInput: map[string]any{"cmd": response}, UpdatedMessages: []chat.Message{{Content: response}}, UpdatedToolResponse: &response,
						}}
						return res, tc.err
					}), nil
				})
				exec := pipelineTestExecutor(t, event, []Hook{
					{Type: HookTypeCommand, Command: `echo '{"hook_specific_output":{"updated_input":{"cmd":"first"},"updated_messages":[{"content":"first"}],"updated_tool_response":"first"}}'`},
					{Type: "test", Command: "fail", OnError: tc.onError},
					{Type: "test", Command: "last"},
				}, registry)
				result, err := exec.Dispatch(t.Context(), event, &Input{
					ToolInput: map[string]any{"cmd": "first"}, Messages: []chat.Message{{Content: "first"}}, ToolResponse: "first",
				})
				require.NoError(t, err)
				assert.True(t, lastRan)
				blocked := tc.blocked && EventContract(event).CanBlock || tc.result.ExitCode == 1 && EventContract(event).FailClosed
				assert.Equal(t, !blocked, result.Allowed)
				switch event {
				case EventPreToolUse, EventToolInputTransform:
					assert.Equal(t, "first", result.ModifiedInput["cmd"])
				case EventBeforeLLMCall:
					require.Len(t, result.UpdatedMessages, 1)
					assert.Equal(t, "first", result.UpdatedMessages[0].Content)
				case EventToolResponseTransform:
					require.NotNil(t, result.UpdatedToolResponse)
					assert.Equal(t, "first", *result.UpdatedToolResponse)
				}
			})
		}
	}
}

func TestPipelineInvalidRewritePreservesDenyAndInput(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	require.NoError(t, registry.RegisterBuiltin("invalid", func(_ context.Context, _ *Input, _ []string) (*Output, error) {
		return &Output{HookSpecificOutput: &HookSpecificOutput{UpdatedInput: map[string]any{"bad": make(chan int)}}}, nil
	}))
	require.NoError(t, registry.RegisterBuiltin("last", func(_ context.Context, in *Input, _ []string) (*Output, error) {
		assert.Equal(t, map[string]any{"cmd": "first", "cwd": "work"}, in.ToolInput)
		return &Output{HookSpecificOutput: &HookSpecificOutput{PermissionDecision: DecisionAllow}}, nil
	}))
	exec := pipelineTestExecutor(t, EventPreToolUse, []Hook{
		{Type: HookTypeCommand, Command: `echo '{"hook_specific_output":{"permission_decision":"deny","permission_decision_reason":"policy","updated_input":{"cmd":"first"}}}'`},
		{Type: HookTypeBuiltin, Command: "invalid"},
		{Type: HookTypeBuiltin, Command: "last"},
	}, registry)
	result, err := exec.Dispatch(t.Context(), EventPreToolUse, &Input{ToolInput: map[string]any{"cmd": "original", "cwd": "work"}})
	require.NoError(t, err)
	assert.False(t, result.Allowed)
	assert.Equal(t, DecisionDeny, result.Decision)
	assert.Equal(t, "policy", result.DecisionReason)
	assert.Contains(t, result.Message, "serialize rewritten input")
	assert.Equal(t, map[string]any{"cmd": "first", "cwd": "work"}, result.ModifiedInput)
}

func TestNonTransformEventsRemainConcurrent(t *testing.T) {
	t.Parallel()

	for _, event := range []EventType{EventSessionStart, EventPermissionRequest, EventBeforeCompaction, EventPreToolUsePreYolo, EventToolGuard} {
		t.Run(string(event), func(t *testing.T) {
			t.Parallel()
			started := make(chan struct{}, 2)
			release := make(chan struct{})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			go func() {
				defer close(release)
				for range 2 {
					select {
					case <-started:
					case <-ctx.Done():
						return
					}
				}
			}()
			registry := NewRegistry()
			require.NoError(t, registry.RegisterBuiltin("wait", func(ctx context.Context, in *Input, args []string) (*Output, error) {
				started <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				assert.Equal(t, "original", in.ToolInput["cmd"])
				return &Output{HookSpecificOutput: &HookSpecificOutput{
					UpdatedInput: map[string]any{"cmd": args[0]}, Summary: args[0], AdditionalContext: args[0],
				}}, nil
			}))
			hookList := []Hook{
				{Type: HookTypeBuiltin, Command: "wait", Args: []string{"first"}},
				{Type: HookTypeBuiltin, Command: "wait", Args: []string{"second"}},
			}
			preempt := true
			exec := NewExecutorWithRegistry(&Config{
				SessionStart: hookList, BeforeCompaction: hookList,
				PermissionRequest: []MatcherConfig{{Hooks: hookList}},
				ToolGuard:         []MatcherConfig{{Hooks: hookList}},
				PreToolUse:        []MatcherConfig{{PreemptYolo: &preempt, Hooks: hookList}},
			}, t.TempDir(), nil, registry)
			result, err := exec.Dispatch(ctx, event, &Input{ToolInput: map[string]any{"cmd": "original"}})
			require.NoError(t, err)
			require.NoError(t, ctx.Err(), "both hooks must start before either finishes")
			if EventContract(event).Context {
				assert.Equal(t, "first\nsecond", result.AdditionalContext)
			} else {
				assert.Empty(t, result.AdditionalContext)
			}
			assert.Nil(t, result.ModifiedInput)
			if event == EventBeforeCompaction {
				assert.Equal(t, "first", result.Summary)
			}
		})
	}
}

type pipelineHandlerFunc func(context.Context, []byte) (HandlerResult, error)

func (f pipelineHandlerFunc) Run(ctx context.Context, input []byte) (HandlerResult, error) {
	return f(ctx, input)
}

func pipelineTestExecutor(t *testing.T, event EventType, hookList []Hook, registry *Registry) *Executor {
	t.Helper()
	cfg := &Config{}
	switch event {
	case EventPreToolUse:
		cfg.PreToolUse = []MatcherConfig{{Hooks: hookList}}
	case EventBeforeLLMCall:
		cfg.BeforeLLMCall = hookList
	case EventToolResponseTransform:
		cfg.ToolResponseTransform = []MatcherConfig{{Hooks: hookList}}
	case EventToolInputTransform:
		cfg.ToolInputTransform = []MatcherConfig{{Hooks: hookList}}
	case EventToolGuard:
		cfg.ToolGuard = []MatcherConfig{{Hooks: hookList}}
	default:
		t.Fatalf("unexpected event: %s", event)
	}
	return NewExecutorWithRegistry(cfg, t.TempDir(), nil, registry)
}

func TestPipelineTimeoutDoesNotCancelNextHook(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		registry := NewRegistry()
		require.NoError(t, registry.RegisterBuiltin("timeout", func(ctx context.Context, _ *Input, _ []string) (*Output, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}))
		ran := false
		require.NoError(t, registry.RegisterBuiltin("next", func(ctx context.Context, in *Input, _ []string) (*Output, error) {
			ran = true
			assert.NoError(t, ctx.Err())
			assert.Equal(t, "original", in.ToolInput["cmd"])
			return &Output{HookSpecificOutput: &HookSpecificOutput{UpdatedInput: map[string]any{"cmd": "next"}}}, nil
		}))
		exec := NewExecutorWithRegistry(&Config{PreToolUse: []MatcherConfig{{Hooks: []Hook{
			{Type: HookTypeBuiltin, Command: "timeout", Timeout: 1},
			{Type: HookTypeBuiltin, Command: "next", Timeout: 1},
		}}}}, "", nil, registry)
		result, err := exec.Dispatch(t.Context(), EventPreToolUse, &Input{ToolInput: map[string]any{"cmd": "original"}})
		require.NoError(t, err)
		assert.True(t, ran)
		assert.False(t, result.Allowed)
		assert.Contains(t, result.Message, "timed out")
		assert.Equal(t, "next", result.ModifiedInput["cmd"])
	})
}

func TestPipelineModelJudgeSeesRewrittenInput(t *testing.T) {
	t.Parallel()

	client := &fakeClient{reply: `{"decision":"ask","reason":"review rewritten command"}`}
	registry := NewRegistry()
	registry.Register(HookTypeModel, NewModelFactory(client))
	exec := pipelineTestExecutor(t, EventPreToolUse, []Hook{
		{Type: HookTypeCommand, Command: `echo '{"hook_specific_output":{"updated_input":{"cmd":"rewritten"}}}'`},
		{Type: HookTypeModel, Model: "test/judge", Prompt: `{{ .ToolInput.cmd }}`, Schema: ShapePreToolUseDecision},
	}, registry)
	result, err := exec.Dispatch(t.Context(), EventPreToolUse, &Input{ToolInput: map[string]any{"cmd": "original"}})
	require.NoError(t, err)
	assert.Equal(t, "rewritten", client.gotUser)
	assert.Equal(t, DecisionAsk, result.Decision)
	assert.Equal(t, "review rewritten command", result.DecisionReason)
}
