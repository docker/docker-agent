//go:build js && wasm

package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall/js"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/embeddedchat"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestSendStreamsTextReasoningAndUsage(t *testing.T) {
	model := newScriptedModel("mock/root", textTurn("Hello!", "thinking"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML})

	var c collectingEmitter
	result, err := s.send("hi", c.emit)
	require.NoError(t, err)

	assert.Equal(t, map[string]any{"role": "assistant", "content": "Hello!"}, result["message"])
	assert.Equal(t, map[string]any{"input_tokens": int64(10), "output_tokens": int64(5)}, result["usage"])
	assert.Equal(t, []string{"delta", "delta", "usage", "finish"}, c.types())
	assert.Equal(t, "thinking", c.events[0]["reasoning"])
	assert.Equal(t, "Hello!", c.events[1]["content"])
	assert.Equal(t, "stop", c.events[3]["reason"])

	require.Len(t, model.lastCall(), 2, "system prompt + user message")
	assert.Equal(t, chat.MessageRoleUser, model.lastCall()[1].Role)
	assert.Equal(t, "hi", model.lastCall()[1].Content)
}

func TestToolCallLoopWithAutoApprove(t *testing.T) {
	echo := &echoToolSet{}
	model := newScriptedModel("mock/root", toolTurn("echo", `{"text":"ping"}`), textTurn("done"))
	s := openTestSession(t, testHost(echo, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML, AutoApprove: true})

	var c collectingEmitter
	result, err := s.send("echo ping", c.emit)
	require.NoError(t, err)

	assert.Equal(t, "done", result["message"].(map[string]any)["content"])
	assert.Equal(t, 1, echo.callCount())
	assert.Empty(t, c.find("tool_confirmation"), "autoApprove must not ask")
	require.Len(t, c.find("tool_call"), 1)
	assert.Equal(t, map[string]any{"type": "tool_call", "id": "call-1", "name": "echo", "args": `{"text":"ping"}`}, c.find("tool_call")[0])
	require.Len(t, c.find("tool_result"), 1)
	assert.Equal(t, "echo: ping", c.find("tool_result")[0]["output"])
	assert.Equal(t, false, c.find("tool_result")[0]["is_error"])
	assert.NotEmpty(t, c.find("tool_call_delta"))
	assert.Equal(t, "finish", c.types()[len(c.types())-1])

	// The tool result went back to the model, keyed by the call ID.
	last := model.lastCall()
	assert.Equal(t, chat.MessageRoleTool, last[len(last)-1].Role)
	assert.Equal(t, "call-1", last[len(last)-1].ToolCallID)
	assert.Equal(t, int64(20), result["usage"].(map[string]any)["input_tokens"], "usage sums both model calls")
}

func TestToolCallAsksForConfirmation(t *testing.T) {
	for _, tc := range []struct {
		decision string
		calls    int
	}{{"approve", 1}, {"reject", 0}} {
		t.Run(tc.decision, func(t *testing.T) {
			echo := &echoToolSet{}
			model := newScriptedModel("mock/root", toolTurn("echo", `{"text":"ping"}`), textTurn("done"))
			s := openTestSession(t, testHost(echo, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML})

			c := collectingEmitter{session: s, decision: tc.decision}
			_, err := s.send("echo ping", c.emit)
			require.NoError(t, err)

			require.Len(t, c.find("tool_confirmation"), 1)
			assert.Equal(t, "echo", c.find("tool_confirmation")[0]["name"])
			assert.Equal(t, tc.calls, echo.callCount())
			if tc.decision == "reject" {
				last := model.lastCall()
				assert.Contains(t, last[len(last)-1].Content, "no thanks", "the model learns why the call was rejected")
			}
		})
	}
}

func TestReadOnlyToolRunsWithoutConfirmation(t *testing.T) {
	echo := &echoToolSet{readOnly: true}
	model := newScriptedModel("mock/root", toolTurn("echo", `{"text":"ping"}`), textTurn("done"))
	s := openTestSession(t, testHost(echo, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML})

	var c collectingEmitter
	_, err := s.send("echo ping", c.emit)
	require.NoError(t, err)
	assert.Empty(t, c.find("tool_confirmation"))
	assert.Equal(t, 1, echo.callCount())
}

func TestUnansweredConfirmationDoesNotHangWhenAborted(t *testing.T) {
	echo := &echoToolSet{}
	model := newScriptedModel("mock/root", toolTurn("echo", `{"text":"ping"}`), textTurn("done"))
	s := openTestSession(t, testHost(echo, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML})

	var c collectingEmitter
	confirmation := make(chan struct{})
	go func() {
		<-confirmation
		s.abort()
	}()
	_, err := s.send("echo ping", func(ctx context.Context, event map[string]any) {
		c.emit(ctx, event)
		if event["type"] == "tool_confirmation" {
			close(confirmation)
		}
	})
	require.ErrorIs(t, err, errAborted)
	assert.Equal(t, 0, echo.callCount())
}

const teamYAML = `
agents:
  root:
    model: mock/root
    instruction: Delegate.
    sub_agents: [helper]
    handoffs: [helper]
  helper:
    model: mock/helper
    description: Helps.
    instruction: Help.
`

func TestTransferTaskReportsHandoffs(t *testing.T) {
	root := newScriptedModel("mock/root", toolTurn("transfer_task", `{"agent":"helper","task":"count to 3","expected_output":"numbers"}`), textTurn("helper said 1 2 3"))
	helper := newScriptedModel("mock/helper", textTurn("1 2 3"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": root, "helper": helper}), sessionOptions{YAML: teamYAML, AutoApprove: true})

	var c collectingEmitter
	result, err := s.send("count", c.emit)
	require.NoError(t, err)

	assert.Equal(t, "helper said 1 2 3", result["message"].(map[string]any)["content"])
	assert.Equal(t, 1, helper.callCount())
	assert.Equal(t, []map[string]any{
		{"type": "handoff", "from": "root", "to": "helper"},
		{"type": "handoff", "from": "helper", "to": "root"},
	}, c.find("handoff"))
}

func TestHandoffReportsAgentSwitch(t *testing.T) {
	root := newScriptedModel("mock/root", toolTurn("handoff", `{"agent":"helper"}`))
	helper := newScriptedModel("mock/helper", textTurn("helper here"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": root, "helper": helper}), sessionOptions{YAML: teamYAML, AutoApprove: true})

	var c collectingEmitter
	result, err := s.send("go", c.emit)
	require.NoError(t, err)

	assert.Equal(t, "helper here", result["message"].(map[string]any)["content"])
	assert.Equal(t, []map[string]any{{"type": "handoff", "from": "root", "to": "helper"}}, c.find("handoff"))

	// The handoff sticks: the next turn goes to helper.
	helper.turns = append(helper.turns, textTurn("still helper"))
	_, err = s.send("again", c.emit)
	require.NoError(t, err)
	assert.Equal(t, 1, root.callCount())
	assert.Equal(t, 2, helper.callCount())
}

func TestFallbackModelIsReported(t *testing.T) {
	const yaml = `
models:
  primary:
    provider: mock
    model: primary
  backup:
    provider: mock
    model: backup
agents:
  root:
    model: primary
    fallback:
      models: [backup]
`
	primary := newScriptedModel("mock/primary", failing(errors.New("primary is down")))
	backup := newScriptedModel("mock/backup", textTurn("backup here"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"primary": primary, "backup": backup}), sessionOptions{YAML: yaml})

	var c collectingEmitter
	result, err := s.send("hi", c.emit)
	require.NoError(t, err)

	assert.Equal(t, "backup here", result["message"].(map[string]any)["content"])
	require.Len(t, c.find("fallback"), 1)
	fallback := c.find("fallback")[0]
	assert.Equal(t, "mock/primary", fallback["from"])
	assert.Equal(t, "mock/backup", fallback["to"])
	assert.Contains(t, fallback["reason"], "primary is down")
}

func TestModelErrorRejectsWithoutFinish(t *testing.T) {
	model := newScriptedModel("mock/root", failing(errors.New("boom")))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML})

	var c collectingEmitter
	_, err := s.send("hi", c.emit)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
	assert.NotEmpty(t, c.find("error"))
	assert.Empty(t, c.find("finish"))
}

func TestAbortCancelsTurnAndKeepsSessionUsable(t *testing.T) {
	started := make(chan struct{})
	model := newScriptedModel("mock/root", blocking(started), textTurn("second"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML})

	go func() {
		<-started
		s.abort()
	}()
	var c collectingEmitter
	_, err := s.send("first", c.emit)
	require.ErrorIs(t, err, errAborted)
	assert.Empty(t, c.find("finish"))

	result, err := s.send("second", c.emit)
	require.NoError(t, err)
	assert.Equal(t, "second", result["message"].(map[string]any)["content"])
}

func TestConcurrentSendIsRejected(t *testing.T) {
	started := make(chan struct{})
	model := newScriptedModel("mock/root", blocking(started))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML})

	firstDone := make(chan error, 1)
	go func() {
		_, err := s.send("first", func(context.Context, map[string]any) {})
		firstDone <- err
	}()
	<-started

	_, err := s.send("second", func(context.Context, map[string]any) {})
	require.ErrorIs(t, err, embeddedchat.ErrRunActive)

	s.abort()
	require.ErrorIs(t, <-firstDone, errAborted)
}

func TestRestartStartsFreshConversation(t *testing.T) {
	model := newScriptedModel("mock/root", textTurn("one"), textTurn("two"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML})

	_, err := s.send("first", func(context.Context, map[string]any) {})
	require.NoError(t, err)
	require.NoError(t, s.restart())

	_, err = s.send("second", func(context.Context, map[string]any) {})
	require.NoError(t, err)
	require.Len(t, model.lastCall(), 2, "the restarted conversation only holds the new prompt")
	assert.Equal(t, "second", model.lastCall()[1].Content)
}

func TestRestartDuringTurnAbortsIt(t *testing.T) {
	started := make(chan struct{})
	model := newScriptedModel("mock/root", blocking(started), textTurn("after restart"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML})

	firstDone := make(chan error, 1)
	go func() {
		_, err := s.send("first", func(context.Context, map[string]any) {})
		firstDone <- err
	}()
	<-started
	require.NoError(t, s.restart())
	require.ErrorIs(t, <-firstDone, errAborted)

	result, err := s.send("second", func(context.Context, map[string]any) {})
	require.NoError(t, err, "restart waits for the aborted turn, so the next send is accepted")
	assert.Equal(t, "after restart", result["message"].(map[string]any)["content"])
}

func TestCloseIsIdempotentAndRejectsSend(t *testing.T) {
	model := newScriptedModel("mock/root")
	s, err := testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}).openSession(t.Context(), sessionOptions{YAML: echoAgentYAML})
	require.NoError(t, err)

	require.NoError(t, s.close())
	require.NoError(t, s.close())
	_, err = s.send("hi", func(context.Context, map[string]any) {})
	require.ErrorIs(t, err, embeddedchat.ErrClosed)
	require.ErrorIs(t, s.ctx.Err(), context.Canceled)
}

func TestHistorySeedsConversationWithToolCalls(t *testing.T) {
	model := newScriptedModel("mock/root", textTurn("I said pong before"))
	history := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "echo ping"},
		{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "call-0", Type: "function", Function: tools.FunctionCall{Name: "echo", Arguments: `{"text":"ping"}`}}}},
		{Role: chat.MessageRoleTool, ToolCallID: "call-0", Content: "echo: ping"},
		{Role: chat.MessageRoleAssistant, Content: "pong"},
	}
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML, History: history})

	result, err := s.send("what did you say?", func(context.Context, map[string]any) {})
	require.NoError(t, err)
	assert.Equal(t, "I said pong before", result["message"].(map[string]any)["content"])

	sent := model.lastCall()
	require.Len(t, sent, 6, "system prompt, four history messages, new prompt")
	assert.Equal(t, "call-0", sent[2].ToolCalls[0].ID)
	assert.Equal(t, "call-0", sent[3].ToolCallID)
	assert.Equal(t, "what did you say?", sent[5].Content)
}

func TestSessionsDoNotShareEnvOrProcessEnv(t *testing.T) {
	var seen []string
	registry := provider.NewRegistry(map[string]provider.Factory{
		"mock": func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, _ ...options.Opt) (provider.Provider, error) {
			secret, _ := env.Get(ctx, "SECRET")
			seen = append(seen, secret)
			return newScriptedModel("mock/" + cfg.Model), nil
		},
	})
	h := host{providers: registry, newToolsets: browserToolsets}

	a := openTestSession(t, h, sessionOptions{YAML: plainAgentYAML, Env: map[string]string{"SECRET": "a"}})
	b := openTestSession(t, h, sessionOptions{YAML: plainAgentYAML, Env: map[string]string{"SECRET": "b"}})

	assert.Equal(t, []string{"a", "b"}, seen)
	assert.Empty(t, os.Getenv("SECRET"), "session env must never reach the process environment")
	assert.NotSame(t, a.chat.Conversation(), b.chat.Conversation())
}

func TestToolProxyIsCarriedByTheSessionContext(t *testing.T) {
	h := testHost(&echoToolSet{}, map[string]provider.Provider{"root": newScriptedModel("mock/root")})

	s := openTestSession(t, h, sessionOptions{YAML: echoAgentYAML, ToolProxy: "https://egress.example.com/proxy"})
	proxy := httpclient.EgressProxyFromContext(s.ctx)
	require.NotNil(t, proxy)
	assert.Equal(t, "egress.example.com", proxy.Host)

	_, err := h.openSession(t.Context(), sessionOptions{YAML: echoAgentYAML, ToolProxy: "http://insecure.example.com"})
	require.ErrorContains(t, err, "must use https")
}

func TestOpenSessionRejectsUnsupportedConfig(t *testing.T) {
	h := testHost(&echoToolSet{}, map[string]provider.Provider{"root": newScriptedModel("mock/root")})
	for name, tc := range map[string]struct {
		yaml string
		opts sessionOptions
		want string
	}{
		"unknown agent": {yaml: echoAgentYAML, opts: sessionOptions{AgentName: "nope"}, want: `agent "nope" not found`},
		"stdio mcp": {want: "stdio MCP servers need a host process", yaml: `
agents:
  root:
    model: mock/root
    toolsets:
      - type: mcp
        command: npx
        args: [server]
`},
		"catalog mcp": {want: "MCP catalog references", yaml: `
agents:
  root:
    model: mock/root
    toolsets:
      - type: mcp
        ref: docker:github
`},
		"remote mcp with local fields": {want: "only apply to local MCP servers", yaml: `
agents:
  root:
    model: mock/root
    toolsets:
      - type: mcp
        remote:
          url: https://mcp.example.com/mcp
        env:
          TOKEN: x
`},
		"command hook": {want: "command hooks are not supported", yaml: `
agents:
  root:
    model: mock/root
    hooks:
      session_start:
        - type: command
          command: echo hi
`},
		"file-based builtin hook": {want: `builtin "add_git_status" is not available`, yaml: `
agents:
  root:
    model: mock/root
    hooks:
      session_start:
        - type: builtin
          command: add_git_status
`},
		"prompt files": {want: "add_prompt_files", yaml: `
agents:
  root:
    model: mock/root
    add_prompt_files: [AGENTS.md]
`},
		"host toolset": {want: `toolset type "shell"`, yaml: `
agents:
  root:
    model: mock/root
    toolsets:
      - type: shell
`},
		"memory path": {want: "memory path: local files", yaml: `
agents:
  root:
    model: mock/root
    toolsets:
      - type: memory
        path: ./memory.db
`},
		"local openapi spec": {want: `openapi url "./openapi.yaml"`, yaml: `
agents:
  root:
    model: mock/root
    toolsets:
      - type: openapi
        url: ./openapi.yaml
`},
		"local openapi spec from env": {want: `openapi url "file:///spec.yaml"`, opts: sessionOptions{Env: map[string]string{"SPEC": "file:///spec.yaml"}}, yaml: `
agents:
  root:
    model: mock/root
    toolsets:
      - type: openapi
        url: ${env.SPEC}
`},
		"harness": {want: `feature "harness"`, yaml: `
agents:
  root:
    harness:
      type: claude-code
`},
		"unregistered provider": {want: `provider "openai"`, yaml: `
agents:
  root:
    model: openai/gpt-4o
`},
		"skills": {want: `feature "skills"`, yaml: `
agents:
  root:
    model: mock/root
    skills: true
`},
	} {
		t.Run(name, func(t *testing.T) {
			opts := tc.opts
			opts.YAML = tc.yaml
			_, err := h.openSession(t.Context(), opts)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestOpenSessionAcceptsRemoteMCPAndBuiltinHooks(t *testing.T) {
	const yaml = `
agents:
  root:
    model: mock/root
    add_date: true
    toolsets:
      - type: mcp
        remote:
          url: https://mcp.example.com/mcp
          headers:
            Authorization: Bearer ${env.MCP_TOKEN}
    hooks:
      turn_start:
        - type: builtin
          command: add_context
          args: ["Be brief."]
`
	h := testHost(&echoToolSet{}, map[string]provider.Provider{"root": newScriptedModel("mock/root")})
	// Nothing connects until the first turn, so opening the session is
	// enough to prove the config passes the browser audit.
	openTestSession(t, h, sessionOptions{YAML: yaml, Env: map[string]string{"MCP_TOKEN": "t"}})
}

func TestBuiltinHookRuns(t *testing.T) {
	const yaml = `
agents:
  root:
    model: mock/root
    hooks:
      turn_start:
        - type: builtin
          command: add_context
          args: ["Be brief."]
`
	model := newScriptedModel("mock/root", textTurn("ok"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: yaml})

	_, err := s.send("hi", func(context.Context, map[string]any) {})
	require.NoError(t, err)
	var system []string
	for _, m := range model.lastCall() {
		if m.Role == chat.MessageRoleSystem {
			system = append(system, m.Content)
		}
	}
	assert.Contains(t, strings.Join(system, "\n"), "Be brief.")
}

func TestCheckRemoteMCPRejectsMissingURL(t *testing.T) {
	require.ErrorContains(t, checkRemoteMCP(latest.Toolset{Type: "mcp"}), "remote.url")
	require.NoError(t, checkRemoteMCP(latest.Toolset{Type: "mcp", Remote: latest.Remote{URL: "https://x"}}))
}

func TestResumeRequest(t *testing.T) {
	req, err := resumeRequest("approve_tool", "echo", "")
	require.NoError(t, err)
	assert.Equal(t, "echo", req.ToolName)
	_, err = resumeRequest("approve_tool", "", "")
	require.Error(t, err)
	_, err = resumeRequest("maybe", "", "")
	require.ErrorContains(t, err, `unknown decision "maybe"`)
	req, err = resumeRequest("reject", "", "why")
	require.NoError(t, err)
	assert.Equal(t, "why", req.Reason)
}

func TestConfirmRejectsWhenNothingIsPending(t *testing.T) {
	echo := &echoToolSet{}
	model := newScriptedModel("mock/root", toolTurn("echo", `{"text":"ping"}`), textTurn("done"))
	s := openTestSession(t, testHost(echo, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML})

	require.ErrorIs(t, s.confirm("approve", "", ""), errNoConfirmation, "before any turn")

	var second error
	c := collectingEmitter{session: s, decision: "approve"}
	_, err := s.send("echo ping", func(ctx context.Context, event map[string]any) {
		if event["type"] == "tool_confirmation" {
			require.NoError(t, s.confirm("approve", "", ""))
			second = s.confirm("approve", "", "")
			return
		}
		c.emit(ctx, event)
	})
	require.NoError(t, err)
	assert.Equal(t, 1, echo.callCount())
	require.ErrorIs(t, second, errNoConfirmation, "a confirmation is answered once")

	require.ErrorIs(t, s.confirm("approve", "", ""), errNoConfirmation, "after the tool completed")
	require.ErrorContains(t, s.confirm("maybe", "", ""), `unknown decision "maybe"`, "bad decisions are reported first")
}

func TestPendingConfirmationClearsWhenTurnIsAborted(t *testing.T) {
	echo := &echoToolSet{}
	model := newScriptedModel("mock/root", toolTurn("echo", `{"text":"ping"}`))
	s := openTestSession(t, testHost(echo, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML})

	_, err := s.send("echo ping", func(_ context.Context, event map[string]any) {
		if event["type"] == "tool_confirmation" {
			s.abort()
		}
	})
	require.ErrorIs(t, err, errAborted)
	require.ErrorIs(t, s.confirm("approve", "", ""), errNoConfirmation)
}

func TestAbortRightAfterStartCancelsTheTurn(t *testing.T) {
	model := newScriptedModel("mock/root", textTurn("never"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML})

	turn, err := s.start("hi")
	require.NoError(t, err)
	s.abort()
	_, err = s.run(turn, func(context.Context, map[string]any) {})
	require.ErrorIs(t, err, errAborted)
	assert.Equal(t, 0, model.callCount(), "the model is never called")
	assert.Empty(t, s.chat.Conversation().GetAllMessages(), "the prompt never joined the conversation")

	turn, err = s.start("again")
	require.NoError(t, err, "the aborted turn is unregistered")
	s.abort()
	_, err = s.run(turn, func(context.Context, map[string]any) {})
	require.ErrorIs(t, err, errAborted)
}

func TestSendIsRejectedWhileRestarting(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	model := newScriptedModel("mock/root", blockingUntil(started, release), textTurn("after restart"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML})

	firstDone := make(chan error, 1)
	go func() {
		_, err := s.send("first", func(context.Context, map[string]any) {})
		firstDone <- err
	}()
	<-started

	restartDone := make(chan error, 1)
	go func() { restartDone <- s.restart() }()
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.restarting
	}, 5*time.Second, time.Millisecond)

	// The old turn is still winding down: nothing may start on top of it.
	_, err := s.send("during", func(context.Context, map[string]any) {})
	require.ErrorIs(t, err, errRestarting)
	require.ErrorIs(t, s.restart(), errRestarting)

	close(release)
	require.ErrorIs(t, <-firstDone, errAborted)
	require.NoError(t, <-restartDone)

	result, err := s.send("second", func(context.Context, map[string]any) {})
	require.NoError(t, err)
	assert.Equal(t, "after restart", result["message"].(map[string]any)["content"])
	require.Len(t, model.lastCall(), 2, "the restarted conversation only holds the new prompt")
}

func TestHandleWithoutOnEventDeclinesConfirmations(t *testing.T) {
	echo := &echoToolSet{}
	model := newScriptedModel("mock/root", toolTurn("echo", `{"text":"ping"}`), textTurn("declined"))
	s := openTestSession(t, testHost(echo, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML})
	h := &sessionHandle{session: s, onEvent: js.Undefined()}

	result, err := s.send("echo ping", h.emit)
	require.NoError(t, err, "nobody can answer, so the confirmation is declined rather than awaited")
	assert.Equal(t, "declined", result["message"].(map[string]any)["content"])
	assert.Equal(t, 0, echo.callCount())
}

func TestSendAfterAbortSettlesQuickly(t *testing.T) {
	started := make(chan struct{})
	model := newScriptedModel("mock/root", blocking(started))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML})

	done := make(chan error, 1)
	go func() {
		_, err := s.send("first", func(context.Context, map[string]any) {})
		done <- err
	}()
	<-started
	s.abort()
	select {
	case err := <-done:
		require.ErrorIs(t, err, errAborted)
	case <-time.After(5 * time.Second):
		t.Fatal("aborted send did not settle")
	}
}
