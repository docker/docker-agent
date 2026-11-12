package hooks

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPromptFileGuardProtocol(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		result HandlerResult
		err    error
		allow  bool
	}{
		{name: "allow", result: HandlerResult{Stdout: `{"continue":true}`}, allow: true},
		{name: "empty"},
		{name: "object", result: HandlerResult{Stdout: `{}`}},
		{name: "plain text", result: HandlerResult{Stdout: "untrusted"}},
		{name: "malformed", result: HandlerResult{Stdout: `{"continue":true`}},
		{name: "unsupported", result: HandlerResult{Stdout: `{"continue":true,"hook_specific_output":{"updated_tool_response":"untrusted"}}`}},
		{name: "deny", result: HandlerResult{Output: &Output{Decision: "block", Reason: "untrusted", SystemMessage: "untrusted"}}},
		{name: "stop", result: HandlerResult{Output: &Output{Continue: new(false), StopReason: "untrusted"}}},
		{name: "error", err: errors.New("untrusted")},
		{name: "exit", result: HandlerResult{ExitCode: 2, Stderr: "untrusted"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := failingHandlerRegistry(tc.result, tc.err)
			reg.Register("allow", NewModelFactory(&fakeClient{reply: `{"decision":"allow","reason":"safe"}`}))
			exec := NewExecutorWithRegistry(&Config{PromptFileGuard: []Hook{
				{Type: "allow", Model: "test/model", Prompt: "check", Schema: ShapeGuardDecision},
				{Type: "fail", OnError: "ignore"},
			}}, "", nil, reg)
			result, err := exec.Dispatch(t.Context(), EventPromptFileGuard, &Input{PromptFile: &PromptFile{Path: "/project/AGENTS.md", Content: "untrusted"}})
			require.NoError(t, err)
			assert.Equal(t, tc.allow, result.Allowed)
			assert.NotContains(t, result.Message, "untrusted")
			assert.Empty(t, result.Stderr)
			assert.Empty(t, result.SystemMessage)
		})
	}
}

func TestPromptFileGuardModel(t *testing.T) {
	t.Parallel()
	for _, reply := range []string{`{"decision":"allow","reason":"safe"}`, `{"decision":"deny","reason":"untrusted"}`, `{}`, ""} {
		t.Run(reply, func(t *testing.T) {
			t.Parallel()
			client := &fakeClient{reply: reply}
			reg := NewRegistry()
			reg.Register(HookTypeModel, NewModelFactory(client))
			exec := NewExecutorWithRegistry(&Config{PromptFileGuard: []Hook{{Type: HookTypeModel, Model: "test/model", Schema: ShapeGuardDecision, SystemPrompt: "policy", Prompt: "{{ .PromptFile | toJSON }}"}}}, "", nil, reg)
			result, err := exec.Dispatch(t.Context(), EventPromptFileGuard, &Input{Source: "loaded", PromptFile: &PromptFile{Path: "/AGENTS.md", Content: "untrusted"}})
			require.NoError(t, err)
			assert.Equal(t, reply == `{"decision":"allow","reason":"safe"}`, result.Allowed)
			assert.JSONEq(t, `{"path":"/AGENTS.md","content":"untrusted"}`, client.gotUser)
			assert.Equal(t, "policy", client.gotSystem)
			require.NotNil(t, client.gotSchema)
			assert.Equal(t, ShapeGuardDecision, client.gotSchema.Name)
			assert.NotContains(t, result.Message, "untrusted")
		})
	}
}

func TestPromptFileGuardTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		reg := NewRegistry()
		require.NoError(t, reg.RegisterBuiltin("wait", func(ctx context.Context, _ *Input, _ []string) (*Output, error) {
			<-ctx.Done()
			return &Output{Continue: new(true)}, nil
		}))
		exec := NewExecutorWithRegistry(&Config{PromptFileGuard: []Hook{{Type: HookTypeBuiltin, Command: "wait", Timeout: 1, OnError: "ignore"}}}, "", nil, reg)
		result, err := exec.Dispatch(t.Context(), EventPromptFileGuard, &Input{})
		require.NoError(t, err)
		assert.False(t, result.Allowed)
	})
}
