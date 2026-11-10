package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
)

func TestACPStdioServer(t *testing.T) {
	if os.Getenv("ACP_STDIO_HELPER") != "1" {
		return
	}
	if marker := os.Getenv("ACP_STARTED"); marker != "" {
		if err := os.WriteFile(marker, []byte("started"), 0o600); err != nil {
			os.Exit(2)
		}
	}
	if os.Getenv("ACP_HANG_INIT") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "acp-test", Version: "1"}, &mcp.ServerOptions{Instructions: "client-provided instructions"})
	if os.Getenv("ACP_FAIL_LIST") == "1" {
		server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				if method == "tools/list" {
					return nil, errors.New("list failed")
				}
				return next(ctx, method, req)
			}
		})
	}
	mcp.AddTool(server, &mcp.Tool{Name: "inspect", Description: "reports subprocess state", Annotations: &mcp.ToolAnnotations{Title: "inspect", ReadOnlyHint: true}},
		func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			if os.Getenv("ACP_ELICIT") == "1" {
				if len(req.Params.InputResponses) == 0 {
					return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{"question": formRequest()}, RequestState: "elicitation-test"}, nil, nil
				}
				result, _ := json.Marshal(req.Params.InputResponses["question"])
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(result)}}}, nil, nil
			}
			wd, _ := os.Getwd()
			value := map[string]any{"cwd": wd, "args": os.Args, "value": os.Getenv("ACP_VALUE"), "inherited": os.Getenv("ACP_INHERITED"), "pid": os.Getpid()}
			data, _ := json.Marshal(value)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "wait", Description: "waits for cancellation", Annotations: &mcp.ToolAnnotations{Title: "wait"}},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			if path := os.Getenv("ACP_CALL_STARTED"); path != "" {
				_ = os.WriteFile(path, []byte("called"), 0o600)
			}
			<-ctx.Done()
			return nil, nil, ctx.Err()
		})
	if os.Getenv("ACP_LONG_NAMES") == "1" {
		for _, name := range []string{strings.Repeat("a", 64), strings.Repeat("a", 63) + "b"} {
			mcp.AddTool(server, &mcp.Tool{Name: name, Annotations: &mcp.ToolAnnotations{Title: name}},
				func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name}}}, nil, nil
				})
		}
	}
	if err := server.Run(t.Context(), &mcp.StdioTransport{}); err != nil && !strings.Contains(err.Error(), "server is closing: EOF") {
		_, _ = fmt.Fprintln(os.Stderr, "helper server:", err)
		os.Exit(2)
	}
	if marker := os.Getenv("ACP_STOPPED"); marker != "" {
		_ = os.WriteFile(marker, []byte("stopped"), 0o600)
	}
	os.Exit(0)
}

func clientServerSpec(t *testing.T, name, value string) acpsdk.McpServer {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	return acpsdk.McpServer{Stdio: &acpsdk.McpServerStdio{
		Name: name, Command: executable,
		Args: []string{"-test.run=^TestACPStdioServer$", "--", "with spaces", "${literal}", "$(literal)"},
		Env:  []acpsdk.EnvVariable{{Name: "ACP_STDIO_HELPER", Value: "1"}, {Name: "ACP_VALUE", Value: value}},
	}}
}

// Callers must defer Stop so subprocesses exit before TempDir cleanup runs.
func clientMCPAgent(t *testing.T) *Agent {
	t.Helper()
	a := NewAgent(nil, &config.RuntimeConfig{}, session.NewInMemorySessionStore())
	a.team = team.New()
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		model := &mockProvider{id: modelsdev.NewID("test", "mcp")}
		root := agent.New("root", "", agent.WithModel(model), agent.WithTools(tools.Tool{Name: "configured"}))
		worker := agent.New("worker", "", agent.WithModel(model))
		agent.WithSubAgents(worker)(root)
		return &teamloader.LoadResult{Team: team.New(team.WithAgents(root, worker))}, nil
	}
	return a
}

func clientTool(t *testing.T, s *Session, suffix string) tools.Tool {
	t.Helper()
	available, err := s.rt.CurrentAgentTools(t.Context())
	require.NoError(t, err)
	for _, tool := range available {
		if tool.Annotations.Title == suffix {
			return tool
		}
	}
	t.Fatalf("client tool %s not found", suffix)
	return tools.Tool{}
}

func inspectClientTool(t *testing.T, tool tools.Tool) map[string]any {
	t.Helper()
	result, err := tool.Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: tool.Name, Arguments: "{}"}}, tools.NopRuntime{})
	require.NoError(t, err)
	require.False(t, result.IsError, result.Output)
	var data map[string]any
	require.NoError(t, json.Unmarshal([]byte(result.Output), &data))
	return data
}

func TestClientMCPNewResumeAndIsolation(t *testing.T) {
	t.Setenv("ACP_INHERITED", "inherited")
	t.Setenv("ACP_VALUE", "parent")
	a := clientMCPAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	wd, other := t.TempDir(), t.TempDir()
	spec := clientServerSpec(t, "human readable name", "first")
	spec.Stdio.Env = append(spec.Stdio.Env, acpsdk.EnvVariable{Name: "ACP_VALUE", Value: "override=value"})
	stopped := filepath.Join(t.TempDir(), "stopped")
	spec.Stdio.Env = append(spec.Stdio.Env, acpsdk.EnvVariable{Name: "ACP_STOPPED", Value: stopped})
	ctx, cancel := context.WithCancel(t.Context())
	created, err := a.NewSession(ctx, acpsdk.NewSessionRequest{Cwd: wd, McpServers: []acpsdk.McpServer{spec}})
	require.NoError(t, err)
	cancel()
	s := a.sessions[string(created.SessionId)]
	originalTeam, originalRuntime := s.team, s.rt
	oldTool := clientTool(t, s, "inspect")
	data := inspectClientTool(t, oldTool)
	expected, err := os.Stat(wd)
	require.NoError(t, err)
	cwd, ok := data["cwd"].(string)
	require.True(t, ok)
	actual, err := os.Stat(cwd)
	require.NoError(t, err)
	assert.True(t, os.SameFile(expected, actual), "subprocess cwd must match the session workspace")
	assert.Equal(t, "override=value", data["value"])
	assert.Equal(t, "inherited", data["inherited"])
	assert.Equal(t, []any{"with spaces", "${literal}", "$(literal)"}, data["args"].([]any)[3:])
	assert.Contains(t, s.clientMCP.Instructions(), "client-provided instructions")
	available, err := s.rt.CurrentAgentTools(t.Context())
	require.NoError(t, err)
	assert.True(t, slices.ContainsFunc(available, func(t tools.Tool) bool { return t.Name == "configured" }))
	require.NoError(t, s.rt.SetCurrentAgent(t.Context(), "worker"))
	assert.Equal(t, data["pid"], inspectClientTool(t, clientTool(t, s, "inspect"))["pid"])
	require.NoError(t, s.rt.SetCurrentAgent(t.Context(), "root"))

	second, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: other, McpServers: []acpsdk.McpServer{clientServerSpec(t, "same name", "second")}})
	require.NoError(t, err)
	secondSession := a.sessions[string(second.SessionId)]
	assert.NotEqual(t, data["pid"], inspectClientTool(t, clientTool(t, secondSession, "inspect"))["pid"])
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd, McpServers: []acpsdk.McpServer{clientServerSpec(t, "human readable name", "replacement")}})
	require.NoError(t, err)
	assert.FileExists(t, stopped)
	assert.Same(t, originalTeam, s.team)
	assert.Same(t, originalRuntime, s.rt)
	newTool := clientTool(t, s, "inspect")
	assert.NotEqual(t, oldTool.Name, newTool.Name)
	s.sess.AppendPermissionAllow(oldTool.Name)
	checker := permissions.NewCheckerFromRules(s.sess.ClonePermissions().Allow, nil, nil)
	assert.NotEqual(t, permissions.Allow, checker.CheckWithArgs(newTool.Name, nil), "remembered approvals must not transfer to replacement servers")
	assert.Equal(t, "replacement", inspectClientTool(t, newTool)["value"])
	stale, err := oldTool.Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: oldTool.Name}}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.True(t, stale.IsError)
	assert.Contains(t, stale.Output, "replaced")
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd})
	require.NoError(t, err)
	available, err = s.rt.CurrentAgentTools(t.Context())
	require.NoError(t, err)
	assert.False(t, slices.ContainsFunc(available, func(t tools.Tool) bool { return strings.HasPrefix(t.Name, "acp_") }))
	_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	assert.Equal(t, "second", inspectClientTool(t, clientTool(t, secondSession, "inspect"))["value"])
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd, McpServers: []acpsdk.McpServer{clientServerSpec(t, "cold", "cold")}})
	require.NoError(t, err)
	assert.Equal(t, "cold", inspectClientTool(t, clientTool(t, a.sessions[string(created.SessionId)], "inspect"))["value"])
}

func TestClientMCPSetupFailurePreservesSession(t *testing.T) {
	t.Parallel()
	a := clientMCPAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	wd, originalRoot, replacementRoot := t.TempDir(), t.TempDir(), t.TempDir()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd, AdditionalDirectories: []string{originalRoot}, McpServers: []acpsdk.McpServer{clientServerSpec(t, "original", "original")}})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	for _, kind := range []string{"missing executable", "list failure", "partial failure"} {
		t.Run(kind, func(t *testing.T) {
			bad := clientServerSpec(t, "bad", "bad")
			if kind == "list failure" {
				bad.Stdio.Env = append(bad.Stdio.Env, acpsdk.EnvVariable{Name: "ACP_FAIL_LIST", Value: "1"})
			} else {
				bad.Stdio.Command = filepath.Join(t.TempDir(), "missing-executable")
			}
			servers := []acpsdk.McpServer{bad}
			stopped := filepath.Join(t.TempDir(), "stopped")
			if kind == "partial failure" {
				good := clientServerSpec(t, "good", "good")
				good.Stdio.Env = append(good.Stdio.Env, acpsdk.EnvVariable{Name: "ACP_STOPPED", Value: stopped})
				servers = append([]acpsdk.McpServer{good}, servers...)
			}
			_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd, AdditionalDirectories: []string{replacementRoot}, McpServers: servers})
			require.Error(t, err)
			assert.Equal(t, "original", inspectClientTool(t, clientTool(t, s, "inspect"))["value"])
			_, roots := s.workspaceSnapshot()
			assert.Equal(t, []string{originalRoot}, roots)
			if kind == "partial failure" {
				assert.FileExists(t, stopped)
			}
		})
	}
}

func TestClientMCPReplacementCancelsAdmittedCalls(t *testing.T) {
	t.Parallel()
	a := clientMCPAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	wd := t.TempDir()
	started := filepath.Join(t.TempDir(), "call-started")
	spec := clientServerSpec(t, "wait", "wait")
	spec.Stdio.Env = append(spec.Stdio.Env, acpsdk.EnvVariable{Name: "ACP_CALL_STARTED", Value: started})
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd, McpServers: []acpsdk.McpServer{spec}})
	require.NoError(t, err)
	tool := clientTool(t, a.sessions[string(created.SessionId)], "wait")
	done := make(chan error, 1)
	go func() {
		_, err := tool.Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: tool.Name, Arguments: "{}"}}, tools.NopRuntime{})
		done <- err
	}()
	require.Eventually(t, func() bool { _, err := os.Stat(started); return err == nil }, 5*time.Second, time.Millisecond)
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd})
	require.NoError(t, err)
	require.Error(t, <-done)
}

func TestClientMCPValidation(t *testing.T) {
	t.Parallel()
	valid := clientServerSpec(t, "server", "value")
	for _, tc := range []struct {
		name    string
		servers []acpsdk.McpServer
	}{
		{"unsupported", []acpsdk.McpServer{{Http: &acpsdk.McpServerHttpInline{Type: "unsupported", Name: "http", Url: "https://example.com"}}}},
		{"empty union", []acpsdk.McpServer{{}}},
		{"duplicate", []acpsdk.McpServer{valid, valid}},
		{"relative executable", []acpsdk.McpServer{{Stdio: &acpsdk.McpServerStdio{Name: "server", Command: "server"}}}},
		{"bad env", []acpsdk.McpServer{{Stdio: &acpsdk.McpServerStdio{Name: "server", Command: valid.Stdio.Command, Env: []acpsdk.EnvVariable{{Name: "A=B", Value: "value"}}}}}},
		{"bad argument", []acpsdk.McpServer{{Stdio: &acpsdk.McpServerStdio{Name: "server", Command: valid.Stdio.Command, Args: []string{"a\x00b"}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := clientMCPAgent(t)
			defer func() { require.NoError(t, a.Stop(t.Context())) }()
			_, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir(), McpServers: tc.servers})
			var rpcErr *acpsdk.RequestError
			require.ErrorAs(t, err, &rpcErr)
			assert.Equal(t, -32602, rpcErr.Code)
			assert.Empty(t, a.sessions)
		})
	}
}

func TestClientMCPCanceledSetupAndClose(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"cancel", "close"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			a := clientMCPAgent(t)
			defer func() { require.NoError(t, a.Stop(t.Context())) }()
			wd, root := t.TempDir(), t.TempDir()
			created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd, AdditionalDirectories: []string{root}})
			require.NoError(t, err)
			marker := filepath.Join(t.TempDir(), "started")
			spec := clientServerSpec(t, "blocked", "blocked")
			spec.Stdio.Env = append(spec.Stdio.Env, acpsdk.EnvVariable{Name: "ACP_STARTED", Value: marker}, acpsdk.EnvVariable{Name: "ACP_HANG_INIT", Value: "1"})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := a.ResumeSession(ctx, acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd, McpServers: []acpsdk.McpServer{spec}})
				done <- err
			}()
			require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil }, 5*time.Second, time.Millisecond)
			if action == "cancel" {
				cancel()
			} else {
				closeCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
				defer stop()
				_, err := a.CloseSession(closeCtx, acpsdk.CloseSessionRequest{SessionId: created.SessionId})
				require.NoError(t, err)
			}
			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("canceled MCP setup did not finish")
			}
			if action == "cancel" {
				s := a.sessions[string(created.SessionId)]
				_, roots := s.workspaceSnapshot()
				assert.Equal(t, []string{root}, roots)
				_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd})
				require.NoError(t, err)
			} else {
				assert.Empty(t, a.sessions)
			}
		})
	}
}

func TestClientMCPBusyResumeStartsNoProcess(t *testing.T) {
	t.Parallel()
	a := clientMCPAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	wd := t.TempDir()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	_, finish, err := s.startTurn(t.Context())
	require.NoError(t, err)
	defer finish()
	spec := clientServerSpec(t, "blocked", "blocked")
	marker := filepath.Join(t.TempDir(), "started")
	spec.Stdio.Env = append(spec.Stdio.Env, acpsdk.EnvVariable{Name: "ACP_STARTED", Value: marker})
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd, McpServers: []acpsdk.McpServer{spec}})
	require.ErrorContains(t, err, "running or pending")
	assert.NoFileExists(t, marker)
}

func TestClientMCPSwapRetiresOldHandlersAtPublication(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	g := &clientMCPGeneration{ctx: ctx, cancel: cancel}
	view := &clientMCPTools{current: g}
	assert.Same(t, g, view.swap(nil))
	_, _, err := g.acquire(t.Context())
	require.Error(t, err, "a cached handler must not acquire the old generation after publication")
	require.NoError(t, g.close(t.Context()))
}

func TestClientMCPCleanupFailureBlocksQueuedPrompt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a := NewAgent(nil, nil, session.NewInMemorySessionStore())
		a.team = team.New()
		s := &Session{id: "failed", sess: session.New(), rt: &fakeRuntime{}}
		_, _, err := registerTestSession(t.Context(), a, s)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		s.clientMCP = &clientMCPTools{current: &clientMCPGeneration{ctx: ctx, cancel: cancel}}
		opCtx, op, err := a.beginSessionConstruction(t.Context(), s.id)
		require.NoError(t, err)
		require.NoError(t, a.reserveReconnect(opCtx, s, false))
		queued := make(chan error, 1)
		go func() {
			_, finish, err := s.startTurn(t.Context())
			if finish != nil {
				finish()
			}
			queued <- err
		}()
		synctest.Wait()
		boom := errors.New("MCP cleanup failed")
		failed := &clientMCPGeneration{closeErr: boom}
		failed.closeOnce.Do(func() {})
		require.ErrorIs(t, a.discardClientMCP(opCtx, op, s, failed), boom)
		s.turns <- struct{}{}
		a.finishOperation(op)
		require.Error(t, <-queued)
		_, _, err = s.startTurn(t.Context())
		require.ErrorIs(t, err, boom)
		require.ErrorIs(t, a.Stop(t.Context()), boom)
	})
}

type clientMCPProvider struct {
	mockProvider

	next   int
	seen   []string
	result string
}

func (p *clientMCPProvider) CreateChatCompletionStream(_ context.Context, messages []chat.Message, available []tools.Tool) (chat.MessageStream, error) {
	p.next++
	if p.next == 1 {
		for _, tool := range available {
			p.seen = append(p.seen, tool.Name)
			if tool.Annotations.Title == "inspect" {
				return &mockStream{responses: []chat.MessageStreamResponse{{Choices: []chat.MessageStreamChoice{{
					Delta:        chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "client-call", Type: "function", Function: tools.FunctionCall{Name: tool.Name, Arguments: "{}"}}}},
					FinishReason: chat.FinishReasonToolCalls,
				}}}}}, nil
			}
		}
	}
	for _, message := range messages {
		if message.Role == chat.MessageRoleTool {
			p.result = message.Content
		}
	}
	return &mockStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "Done"}}}},
		{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}},
	}}, nil
}

func TestClientMCPToolsReachRuntime(t *testing.T) {
	t.Parallel()
	a := clientMCPAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	prov := &clientMCPProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "client-mcp")}}
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		return &teamloader.LoadResult{Team: team.New(team.WithAgents(agent.New("root", "test", agent.WithModel(prov))))}, nil
	}
	fixture := newRunAgentFixture(t, &fakeRuntime{}, &captureWriter{})
	a.SetAgentConnection(fixture.agent.conn)
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acpsdk.McpServer{clientServerSpec(t, "runtime", "reached model")}})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	s.sess.ToolsApproved = true
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	response, err := a.Prompt(ctx, acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("inspect environment")}})
	require.NoError(t, err)
	assert.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
	assert.Contains(t, prov.result, "reached model")
	assert.Equal(t, "Done", s.sess.GetLastAssistantMessageContent())
}

func TestClientMCPHonorsAgentReadOnly(t *testing.T) {
	t.Parallel()
	a := clientMCPAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	load := a.loadTeam
	a.loadTeam = func(ctx context.Context, wd string) (*teamloader.LoadResult, error) {
		result, err := load(ctx, wd)
		if err == nil {
			team.WithAgentConfigs(map[string]latest.AgentConfig{"root": {ReadOnly: true}})(result.Team)
		}
		return result, err
	}
	wd := t.TempDir()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd, McpServers: []acpsdk.McpServer{clientServerSpec(t, "tools", "first")}})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	for turn := range 2 {
		if turn > 0 {
			_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd, McpServers: []acpsdk.McpServer{clientServerSpec(t, "tools", "second")}})
			require.NoError(t, err)
		}
		for _, name := range []string{"root", "worker"} {
			require.NoError(t, s.rt.SetCurrentAgent(t.Context(), name))
			available, err := s.rt.CurrentAgentTools(t.Context())
			require.NoError(t, err)
			assert.True(t, slices.ContainsFunc(available, func(tool tools.Tool) bool { return tool.Annotations.Title == "inspect" }))
			assert.Equal(t, name == "worker", slices.ContainsFunc(available, func(tool tools.Tool) bool { return tool.Annotations.Title == "wait" }))
		}
	}
}

func TestClientMCPLongToolNamesDispatchExactly(t *testing.T) {
	t.Parallel()
	a := clientMCPAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	spec := clientServerSpec(t, "long names", "long")
	spec.Stdio.Env = append(spec.Stdio.Env, acpsdk.EnvVariable{Name: "ACP_LONG_NAMES", Value: "1"})
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acpsdk.McpServer{spec}})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	seen := make(map[string]bool)
	for _, name := range []string{strings.Repeat("a", 64), strings.Repeat("a", 63) + "b"} {
		tool := clientTool(t, s, name)
		assert.LessOrEqual(t, len(tool.Name), 64)
		assert.False(t, seen[tool.Name])
		seen[tool.Name] = true
		result, err := tool.Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: tool.Name, Arguments: "{}"}}, tools.NopRuntime{})
		require.NoError(t, err)
		assert.Equal(t, name, result.Output)
	}
}

func TestClientMCPFailedSessionRejectsAlreadyAdmittedResume(t *testing.T) {
	t.Parallel()
	a := NewAgent(nil, nil, session.NewInMemorySessionStore())
	a.team = team.New()
	s := &Session{id: "failed", sess: session.New(), rt: &fakeRuntime{}, clientMCP: &clientMCPTools{}}
	_, _, err := registerTestSession(t.Context(), a, s)
	require.NoError(t, err)
	ctx, op, err := a.beginSessionConstruction(t.Context(), s.id)
	require.NoError(t, err)
	boom := errors.New("earlier cleanup failed")
	s.mu.Lock()
	s.failed = boom
	s.mu.Unlock()
	marker := filepath.Join(t.TempDir(), "started")
	spec := clientServerSpec(t, "must not start", "value")
	spec.Stdio.Env = append(spec.Stdio.Env, acpsdk.EnvVariable{Name: "ACP_STARTED", Value: marker})
	servers, err := validateClientMCPServers([]acpsdk.McpServer{spec})
	require.NoError(t, err)
	err = a.reconnectRegisteredSession(ctx, s, "", nil, servers, op, false, nil)
	require.ErrorIs(t, err, boom)
	a.finishOperation(op)
	assert.NoFileExists(t, marker)
	require.NoError(t, a.Stop(t.Context()))
}

func TestClientMCPPersistenceFailureStopsChild(t *testing.T) {
	t.Parallel()
	a := clientMCPAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	a.sessionStore = failingAddStore{session.NewInMemorySessionStore()}
	stopped := filepath.Join(t.TempDir(), "stopped")
	spec := clientServerSpec(t, "rollback", "value")
	spec.Stdio.Env = append(spec.Stdio.Env, acpsdk.EnvVariable{Name: "ACP_STOPPED", Value: stopped})
	_, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acpsdk.McpServer{spec}})
	require.ErrorContains(t, err, "store failed")
	assert.FileExists(t, stopped)
	assert.Empty(t, a.sessions)
}

type clientMCPBackgroundProvider struct {
	mockProvider

	called bool
}

func (p *clientMCPBackgroundProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	if !p.called {
		p.called = true
		return &mockStream{responses: []chat.MessageStreamResponse{{Choices: []chat.MessageStreamChoice{{
			Delta:        chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "dispatch", Type: "function", Function: tools.FunctionCall{Name: "run_background_agent", Arguments: `{"agent":"worker","task":"call wait"}`}}}},
			FinishReason: chat.FinishReasonToolCalls,
		}}}}}, nil
	}
	return &mockStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "dispatched"}}}},
		{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}},
	}}, nil
}

type clientMCPWaitProvider struct{ mockProvider }

func (*clientMCPWaitProvider) CreateChatCompletionStream(_ context.Context, _ []chat.Message, available []tools.Tool) (chat.MessageStream, error) {
	for _, tool := range available {
		if tool.Annotations.Title == "wait" {
			return &mockStream{responses: []chat.MessageStreamResponse{{Choices: []chat.MessageStreamChoice{{
				Delta:        chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "wait-call", Type: "function", Function: tools.FunctionCall{Name: tool.Name, Arguments: "{}"}}}},
				FinishReason: chat.FinishReasonToolCalls,
			}}}}}, nil
		}
	}
	return nil, errors.New("client wait tool not available")
}

func TestClientMCPCloseStopsBackgroundCall(t *testing.T) {
	t.Parallel()
	a := clientMCPAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		worker := agent.New("worker", "test", agent.WithModel(&clientMCPWaitProvider{mockProvider{id: modelsdev.NewID("test", "worker")}}))
		root := agent.New("root", "test", agent.WithModel(&clientMCPBackgroundProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "root")}}), agent.WithSubAgents(worker), agent.WithToolSets(agenttool.New()))
		return &teamloader.LoadResult{Team: team.New(team.WithAgents(root, worker))}, nil
	}
	fixture := newRunAgentFixture(t, &fakeRuntime{}, &captureWriter{})
	a.SetAgentConnection(fixture.agent.conn)
	markers := t.TempDir()
	spec := clientServerSpec(t, "background", "value")
	spec.Stdio.Env = append(spec.Stdio.Env, acpsdk.EnvVariable{Name: "ACP_CALL_STARTED", Value: filepath.Join(markers, "called")}, acpsdk.EnvVariable{Name: "ACP_STOPPED", Value: filepath.Join(markers, "stopped")})
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acpsdk.McpServer{spec}})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	s.sess.ToolsApproved = true
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = a.Prompt(ctx, acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("dispatch")}})
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(markers, "called")); return err == nil }, 5*time.Second, time.Millisecond)
	_, err = a.CloseSession(ctx, acpsdk.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(markers, "stopped"))
}
