package runtime

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/random"
	"github.com/docker/docker-agent/pkg/tools/codemode"
)

func TestRunStream_RepeatedRandomDraws(t *testing.T) {
	t.Parallel()

	for _, codeMode := range []bool{false, true} {
		t.Run(fmt.Sprintf("code_mode=%t", codeMode), func(t *testing.T) {
			t.Parallel()

			const draws = 6
			streams := make([]chat.MessageStream, 0, draws+1)
			for i := range draws {
				callID := fmt.Sprintf("roll_%d", i)
				streams = append(streams, newStreamBuilder().
					AddToolCallName(callID, random.ToolNameRandomInt).
					AddToolCallArguments(callID, `{"min":1,"max":6}`).
					AddToolCallStopWithUsage(2, 2).
					Build())
			}
			streams = append(streams, newStreamBuilder().AddContent("rolled six dice").AddStopWithUsage(2, 2).Build())
			prov := &queueProvider{id: "test/mock-model", streams: streams}
			ts := tools.ToolSet(random.New())
			if codeMode {
				ts = codemode.Wrap(ts)
			}
			root := agent.New("root", "dice roller", agent.WithModel(prov), agent.WithToolSets(ts))
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
				WithSessionCompaction(false), WithModelStore(mockModelStore{}))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close()) })
			sess := session.New(session.WithUserMessage("roll six dice"))
			sess.Title = "Unit Test"

			for event := range rt.RunStream(t.Context(), sess) {
				_, failed := event.(*ErrorEvent)
				assert.False(t, failed, "unexpected error event: %v", event)
			}

			assert.Equal(t, "rolled six dice", sess.GetLastAssistantMessageContent())
			assert.Empty(t, prov.streams)
			for i := range draws {
				output := toolResultContent(t, sess, fmt.Sprintf("roll_%d", i))
				value, err := strconv.ParseInt(output, 10, 64)
				require.NoError(t, err)
				assert.GreaterOrEqual(t, value, int64(1))
				assert.LessOrEqual(t, value, int64(6))
			}
		})
	}
}

func TestRunStream_FailedRandomDrawsTriggerLoopDetection(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		args        string
		unavailable bool
		denied      bool
		rewrite     bool
		collision   bool
		codeMode    bool
	}{
		{name: "missing bounds", args: `{}`},
		{name: "reversed bounds", args: `{"min":6,"max":1}`},
		{name: "code mode missing bounds", args: `{}`, codeMode: true},
		{name: "code mode reversed bounds", args: `{"min":6,"max":1}`, codeMode: true},
		{name: "code mode denied", args: `{"min":1,"max":6}`, codeMode: true, denied: true},
		{name: "malformed arguments", args: `{"min":1,"max":`},
		{name: "unavailable", args: `{"min":1,"max":6}`, unavailable: true},
		{name: "denied", args: `{"min":1,"max":6}`, denied: true},
		{name: "rewritten invalid bounds", args: `{"min":1,"max":6}`, rewrite: true},
		{name: "same-name custom tool", args: `{"min":1,"max":6}`, collision: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			streams := make([]chat.MessageStream, 0, 6)
			for i := range 6 {
				id := fmt.Sprintf("call_%d", i)
				streams = append(streams, newStreamBuilder().
					AddToolCallName(id, random.ToolNameRandomInt).
					AddToolCallArguments(id, tc.args).
					AddToolCallStopWithUsage(2, 2).Build())
			}
			prov := &queueProvider{id: "test/mock-model", streams: streams}
			opts := []agent.Opt{agent.WithModel(prov)}
			if !tc.unavailable {
				ts := tools.ToolSet(random.New())
				if tc.codeMode {
					ts = codemode.Wrap(ts)
				}
				opts = append(opts, agent.WithToolSets(ts))
			}
			if tc.collision {
				opts = []agent.Opt{agent.WithModel(prov), agent.WithToolSets(newStubToolSet(nil, []tools.Tool{{
					Name: random.ToolNameRandomInt,
					Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
						return tools.ResultSuccess("custom result"), nil
					},
				}}, nil))}
			}
			if tc.rewrite {
				opts = append(opts, agent.WithHooks(&latest.HooksConfig{
					ToolInputTransform: []latest.HookMatcherConfig{{Matcher: random.ToolNameRandomInt, Hooks: []latest.HookDefinition{{Type: "builtin", Command: "invalid_bounds"}}}},
				}))
			}
			root := agent.New("root", "dice roller", opts...)
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close()) })
			if tc.rewrite {
				require.NoError(t, rt.hooksRegistry.RegisterBuiltin("invalid_bounds", func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
					return &hooks.Output{HookSpecificOutput: &hooks.HookSpecificOutput{UpdatedInput: map[string]any{"min": 6, "max": 1}}}, nil
				}))
			}
			sess := session.New(session.WithUserMessage("roll"), session.WithToolsApproved(true))
			sess.Title = "Unit Test"
			if tc.denied {
				sess.Permissions = &session.PermissionsConfig{Deny: []string{random.ToolNameRandomInt}}
			}
			var loopError *ErrorEvent
			responses := 0
			for event := range rt.RunStream(t.Context(), sess) {
				if e, ok := event.(*ErrorEvent); ok && e.Code == ErrorCodeLoopDetected {
					loopError = e
				}
				if e, ok := event.(*ToolCallResponseEvent); ok {
					responses++
					assert.Equal(t, !tc.collision, e.Result.IsError)
				}
			}
			require.NotNil(t, loopError)
			assert.Equal(t, 5, responses)
			assert.Len(t, prov.streams, 1)
		})
	}
}

func TestRunStream_RandomDrawsRespectIterationLimit(t *testing.T) {
	t.Parallel()

	var streams []chat.MessageStream
	for i := range 6 {
		id := fmt.Sprintf("roll_%d", i)
		streams = append(streams, newStreamBuilder().AddToolCallName(id, random.ToolNameRandomInt).
			AddToolCallArguments(id, `{"min":1,"max":6}`).AddToolCallStopWithUsage(2, 2).Build())
	}
	prov := &queueProvider{id: "test/mock-model", streams: streams}
	root := agent.New("root", "dice roller", agent.WithModel(prov), agent.WithToolSets(random.New()))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	sess := session.New(session.WithUserMessage("roll"), session.WithMaxIterations(3))
	sess.NonInteractive = true
	sess.Title = "Unit Test"
	for range rt.RunStream(t.Context(), sess) {
	}
	assert.Len(t, prov.streams, 3)
}
