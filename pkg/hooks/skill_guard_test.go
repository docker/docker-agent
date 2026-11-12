package hooks

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestSkillGuardExplicitVerdicts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		result  HandlerResult
		err     error
		allowed bool
	}{
		{name: "allow", result: HandlerResult{Stdout: `{"continue":true}`}, allowed: true},
		{name: "block", result: HandlerResult{Stdout: `{"decision":"block","reason":"untrusted body"}`}},
		{name: "stop", result: HandlerResult{Stdout: `{"continue":false,"stop_reason":"untrusted body"}`}},
		{name: "exit 2", result: HandlerResult{ExitCode: 2, Stderr: "untrusted body"}},
		{name: "empty"},
		{name: "object", result: HandlerResult{Stdout: `{}`}},
		{name: "text", result: HandlerResult{Stdout: "untrusted body"}},
		{name: "invalid", result: HandlerResult{Stdout: `{"continue":true`}},
		{name: "unknown field", result: HandlerResult{Stdout: `{"continue":true,"extra":"untrusted body"}`}},
		{name: "rewrite", result: HandlerResult{Stdout: `{"continue":true,"hook_specific_output":{"updated_tool_response":"untrusted body"}}`}},
		{name: "permission", result: HandlerResult{Stdout: `{"hook_specific_output":{"permission_decision":"allow"}}`}},
		{name: "error", err: errors.New("untrusted body")},
		{name: "exit 1", result: HandlerResult{ExitCode: 1, Stderr: "untrusted body"}},
		{name: "echo", result: HandlerResult{Output: &Output{Decision: "block", Reason: "untrusted body", SystemMessage: "untrusted body"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := failingHandlerRegistry(tc.result, tc.err)
			reg.Register("allow", NewModelFactory(&fakeClient{reply: `{"decision":"allow","reason":"ok"}`}))
			exec := NewExecutorWithRegistry(&Config{SkillContentGuard: []Hook{
				{Type: "allow", Model: "test/model", Prompt: "check", Schema: ShapeGuardDecision},
				{Type: "fail", OnError: "ignore"},
			}}, "", nil, reg)
			result, err := exec.Dispatch(t.Context(), EventSkillContentGuard, &Input{Skill: &tools.SkillContent{Content: "untrusted body"}})
			require.NoError(t, err)
			assert.Equal(t, tc.allowed, result.Allowed)
			assert.NotContains(t, result.Message, "untrusted body")
			assert.Empty(t, result.SystemMessage)
			assert.Empty(t, result.Stderr)
			assert.Empty(t, result.AdditionalContext)
		})
	}
}

func TestSkillGuardModel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reply   string
		allowed bool
	}{
		{`{"decision":"allow","reason":"safe"}`, true},
		{`{"decision":"deny","reason":"untrusted body"}`, false},
		{`{"decision":"ask","reason":"unsure"}`, false},
		{`{"decision":"allow"}`, false},
		{`{"decision":"allow","reason":null}`, false},
		{`{"decision":"allow","reason":"ok","extra":1}`, false},
		{`{"decision":"allow","reason":"ok"} {}`, false},
		{"", false},
		{"```json\n{}\n```", false},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			t.Parallel()
			client := &fakeClient{reply: tc.reply}
			reg := NewRegistry()
			reg.Register(HookTypeModel, NewModelFactory(client))
			exec := NewExecutorWithRegistry(&Config{SkillContentGuard: []Hook{{Type: HookTypeModel, Model: "test/model", Prompt: "{{ .Skill.Content | toJSON }}", SystemPrompt: "Literal policy {{ .Skill.Content }}", Schema: ShapeGuardDecision}}}, "", nil, reg)
			result, err := exec.Dispatch(t.Context(), EventSkillContentGuard, &Input{Skill: &tools.SkillContent{Content: "untrusted body"}})
			require.NoError(t, err)
			assert.Equal(t, tc.allowed, result.Allowed)
			assert.NotContains(t, result.Message, "untrusted body")
			assert.Equal(t, `"untrusted body"`, client.gotUser)
			assert.Equal(t, "Literal policy {{ .Skill.Content }}", client.gotSystem)
			require.NotNil(t, client.gotSchema)
			assert.Equal(t, ShapeGuardDecision, client.gotSchema.Name)
		})
	}
}

func TestGuardDecisionOtherEvents(t *testing.T) {
	t.Parallel()
	out, err := guardDecisionShape(`{"decision":"deny","reason":"blocked"}`, &Input{HookEventName: EventPostToolUse})
	require.NoError(t, err)
	assert.True(t, out.IsBlocked())
	assert.Equal(t, "blocked", out.Reason)
	_, err = guardDecisionShape(`{"decision":"allow","reason":"ok"}`, &Input{HookEventName: EventTurnStart})
	require.Error(t, err)
}

func TestSkillGuardTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		reg := NewRegistry()
		require.NoError(t, reg.RegisterBuiltin("wait", func(ctx context.Context, _ *Input, _ []string) (*Output, error) {
			<-ctx.Done()
			return &Output{Continue: new(true)}, nil
		}))
		exec := NewExecutorWithRegistry(&Config{SkillContentGuard: []Hook{{Type: HookTypeBuiltin, Command: "wait", Timeout: 1, OnError: "ignore"}}}, "", nil, reg)
		start := time.Now()
		result, err := exec.Dispatch(t.Context(), EventSkillContentGuard, &Input{})
		require.NoError(t, err)
		assert.False(t, result.Allowed)
		assert.Equal(t, time.Second, time.Since(start))
	})
}

func TestGuardDecisionRejectsAmbiguousJSON(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`{"decision":"deny","decision":"allow","reason":"safe"}`,
		`{"decision":"deny","Decision":"allow","reason":"safe"}`,
		`{"Decision":"allow","Reason":"safe"}`,
		`{"decision":"allow","reason":"unsafe","reason":"safe"}`,
		`{"decision":"allow","reason":null}`,
		`{"decision":"allow","reason":{}}`,
		`{"decision":"allow","reason":1}`,
		`{"decision":"allow","reason":true}`,
		`{"decision":"allow","reason":"safe"} trailing`,
		`[]`, `null`,
	} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			for _, event := range []EventType{EventSkillContentGuard, EventPromptFileGuard} {
				out, err := guardDecisionShape(raw, &Input{HookEventName: event})
				require.Error(t, err)
				assert.Nil(t, out)
			}
		})
	}
}

func TestContentGuardCommandRejectsAmbiguousJSON(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`{"continue":false,"continue":true}`,
		`{"continue":false,"Continue":true}`,
		`{"Continue":true}`,
		`{"continue":true,"decision":"block","decision":""}`,
		`{"continue":true,"Decision":"block"}`,
		`{"continue":true,"hook_specific_output":{"HookEventName":"skill_content_guard"}}`,
		`{"continue":true,"hook_specific_output":{"hook_event_name":"wrong","hook_event_name":"skill_content_guard"}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			for _, event := range []EventType{EventSkillContentGuard, EventPromptFileGuard} {
				cfg := &Config{}
				defs := []Hook{{Type: "fail", OnError: "ignore"}}
				if event == EventSkillContentGuard {
					cfg.SkillContentGuard = defs
				} else {
					cfg.PromptFileGuard = defs
				}
				exec := NewExecutorWithRegistry(cfg, "", nil, failingHandlerRegistry(HandlerResult{Stdout: raw}, nil))
				result, err := exec.Dispatch(t.Context(), event, &Input{})
				require.NoError(t, err)
				assert.False(t, result.Allowed)
			}
		})
	}
	// This stricter protocol does not change unrelated legacy hooks.
	out, err := parseStdoutJSON(`{"continue":false,"Continue":true}`, false)
	require.NoError(t, err)
	assert.True(t, out.ShouldContinue())
}
