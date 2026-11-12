package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/internal/portcullistest"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/shell"
)

type terminalRequest struct {
	method string
	params json.RawMessage
	id     json.RawMessage
}

type terminalPeer struct {
	output  captureWriter
	peer    io.Writer
	mu      sync.Mutex
	calls   []terminalRequest
	respond func(terminalRequest) (any, *acpsdk.RequestError)
	other   io.Writer
}

func (p *terminalPeer) Write(data []byte) (int, error) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return 0, err
	}
	if !strings.HasPrefix(msg.Method, "terminal/") {
		if p.other != nil {
			return p.other.Write(data)
		}
		return p.output.Write(data)
	}
	req := terminalRequest{method: msg.Method, params: msg.Params, id: msg.ID}
	p.mu.Lock()
	p.calls = append(p.calls, req)
	p.mu.Unlock()
	var result any
	var rpcErr *acpsdk.RequestError
	if p.respond != nil {
		result, rpcErr = p.respond(req)
	} else {
		switch req.method {
		case "terminal/create":
			result = acpsdk.CreateTerminalResponse{TerminalId: "terminal-1"}
		case "terminal/wait_for_exit":
			result = acpsdk.WaitForTerminalExitResponse{ExitCode: new(0)}
		case "terminal/output":
			result = acpsdk.TerminalOutputResponse{Output: "output"}
		default:
			result = map[string]any{}
		}
	}
	if result == nil && rpcErr == nil {
		return len(data), nil
	}
	if err := p.reply(req.id, result, rpcErr); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (p *terminalPeer) reply(id json.RawMessage, result any, rpcErr *acpsdk.RequestError) error {
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		msg["error"] = rpcErr
	} else {
		msg["result"] = result
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = p.peer.Write(append(data, '\n'))
	return err
}

func (p *terminalPeer) snapshot() []terminalRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func (p *terminalPeer) methods() []string {
	var names []string
	for _, r := range p.snapshot() {
		names = append(names, r.method)
	}
	return names
}

func newTerminalFixture(t *testing.T, cfg latest.Toolset) (*terminalToolset, *Session, context.Context, *terminalPeer) {
	t.Helper()
	return terminalFixtureForSession(t, cfg, &Session{})
}

func terminalFixtureForSession(t *testing.T, cfg latest.Toolset, owner *Session) (*terminalToolset, *Session, context.Context, *terminalPeer) {
	t.Helper()
	wd := t.TempDir()
	a := &Agent{clientTerminal: true}
	reader, writer := io.Pipe()
	peer := &terminalPeer{peer: writer}
	conn := a.NewConnection(peer, reader)
	conn.SetLogger(slog.New(slog.DiscardHandler))
	owner.terminals = newTerminalManager(conn, "owner")
	t.Cleanup(func() { <-owner.close(t.Context()); _ = writer.Close(); <-conn.Done() })
	rc := &config.RuntimeConfig{Config: config.Config{WorkingDir: wd}, EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{"CONFIGURED": "expanded=value"})}
	toolset, err := newTerminalToolset(t.Context(), cfg, rc)
	require.NoError(t, err)
	return toolset.(*terminalToolset), owner, context.WithValue(t.Context(), terminalOwnerKey{}, owner.terminals), peer
}

func callTerminalTool(t *testing.T, ctx context.Context, ts *terminalToolset, args string) *tools.ToolCallResult {
	t.Helper()
	available, err := ts.Tools(ctx)
	require.NoError(t, err)
	require.Len(t, available, 1)
	result, err := available[0].Handler(ctx, tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "shell", Arguments: args}}, tools.NopRuntime{})
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}

func TestClientTerminalShellRequestAndEnvironment(t *testing.T) {
	t.Setenv("UNCONFIGURED_HOST_SECRET", "must-not-be-sent")
	ts, _, ctx, peer := newTerminalFixture(t, latest.Toolset{Env: map[string]string{"VALUE": "${CONFIGURED}", "EMPTY": ""}})
	result := callTerminalTool(t, ctx, ts, `{"cmd":"printf '%s' value | cat","command":"ignored","CMD":"never","cwd":"nested"}`)
	assert.False(t, result.IsError, result.Output)
	assert.Equal(t, "output", result.Output)
	assert.Equal(t, []string{"terminal/create", "terminal/wait_for_exit", "terminal/output", "terminal/release"}, peer.methods())
	var request acpsdk.CreateTerminalRequest
	require.NoError(t, json.Unmarshal(peer.snapshot()[0].params, &request))
	command, prefix := terminalInterpreter()
	assert.Equal(t, command, request.Command)
	assert.Equal(t, append(prefix, "printf '%s' value | cat"), request.Args)
	require.NotNil(t, request.Cwd)
	assert.Equal(t, filepath.Join(ts.workingDir, "nested"), *request.Cwd)
	assert.Equal(t, []acpsdk.EnvVariable{{Name: "EMPTY", Value: ""}, {Name: "VALUE", Value: "expanded=value"}}, request.Env)
	assert.NotContains(t, string(peer.snapshot()[0].params), "must-not-be-sent")
	require.NotNil(t, request.OutputByteLimit)
	assert.Equal(t, terminalOutputLimit, *request.OutputByteLimit)
	for _, call := range peer.snapshot() {
		var params map[string]any
		require.NoError(t, json.Unmarshal(call.params, &params))
		assert.Equal(t, "owner", params["sessionId"])
	}
}

func TestClientTerminalShellAliasAndValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args  string
		valid bool
		cmd   string
	}{
		{args: `{"command":"echo alias"}`, valid: true, cmd: "echo alias"},
		{args: `{"cmd":"echo canonical","CMD":"wrong"}`, valid: true, cmd: "echo canonical"},
		{args: `{"CMD":"wrong"}`},
		{args: `{"cmd":" "}`},
		{args: `{"cmd":"echo ok","timeout":9223372037}`},
	} {
		ts, _, ctx, peer := newTerminalFixture(t, latest.Toolset{})
		result := callTerminalTool(t, ctx, ts, tc.args)
		if tc.valid {
			assert.False(t, result.IsError)
			var req acpsdk.CreateTerminalRequest
			require.NoError(t, json.Unmarshal(peer.snapshot()[0].params, &req))
			assert.Equal(t, tc.cmd, req.Args[len(req.Args)-1])
		} else {
			assert.True(t, result.IsError)
			assert.Empty(t, peer.snapshot())
		}
	}
	ts, _, ctx, peer := newTerminalFixture(t, latest.Toolset{SudoAskpass: new(true)})
	result := callTerminalTool(t, ctx, ts, `{"cmd":"sudo command"}`)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "sudo_askpass")
	assert.Empty(t, peer.snapshot())
}

func TestClientTerminalOutputAndFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, output string
		exit         *int
		signal       *string
		truncated    bool
		outputError  bool
		want         string
		isError      bool
	}{
		{name: "empty", exit: new(0), want: "<no output>"},
		{name: "nonzero", exit: new(7), output: "failure", want: "exit status 7"},
		{name: "signal", signal: new("SIGTERM"), output: "partial", want: "signal SIGTERM"},
		{name: "no status", output: "partial", want: "no exit status", isError: true},
		{name: "client truncation", exit: new(0), output: "tail", truncated: true, want: "truncated"},
		{name: "oversize tail", exit: new(0), output: strings.Repeat("é", terminalOutputLimit) + "END", want: "END"},
		{name: "output failure", exit: new(0), outputError: true, want: "Error reading", isError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts, _, ctx, peer := newTerminalFixture(t, latest.Toolset{})
			peer.respond = func(r terminalRequest) (any, *acpsdk.RequestError) {
				switch r.method {
				case "terminal/create":
					return acpsdk.CreateTerminalResponse{TerminalId: "id"}, nil
				case "terminal/wait_for_exit":
					return acpsdk.WaitForTerminalExitResponse{ExitCode: tc.exit, Signal: tc.signal}, nil
				case "terminal/output":
					if tc.outputError {
						return nil, acpsdk.NewInternalError("output failed")
					}
					return acpsdk.TerminalOutputResponse{Output: tc.output, Truncated: tc.truncated}, nil
				default:
					return map[string]any{}, nil
				}
			}
			result := callTerminalTool(t, ctx, ts, `{"cmd":"command"}`)
			assert.Equal(t, tc.isError, result.IsError, result.Output)
			assert.Contains(t, result.Output, tc.want)
			assert.Less(t, len(result.Output), terminalOutputLimit+256)
			assert.Equal(t, "terminal/release", peer.methods()[len(peer.methods())-1])
		})
	}
}

func TestClientTerminalCancellationWaitAndLateCreate(t *testing.T) {
	t.Parallel()
	for _, lateCreate := range []bool{false, true} {
		t.Run(map[bool]string{false: "wait", true: "create"}[lateCreate], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ts, _, base, peer := newTerminalFixture(t, latest.Toolset{})
				ctx, cancel := context.WithCancel(base)
				defer cancel()
				entered := make(chan terminalRequest, 1)
				peer.respond = func(r terminalRequest) (any, *acpsdk.RequestError) {
					switch r.method {
					case "terminal/create":
						if lateCreate {
							entered <- r
							return nil, nil
						}
						return acpsdk.CreateTerminalResponse{TerminalId: "late"}, nil
					case "terminal/wait_for_exit":
						entered <- r
						return nil, nil
					case "terminal/kill":
						return nil, acpsdk.NewInternalError("kill failed")
					case "terminal/output":
						return acpsdk.TerminalOutputResponse{Output: "partial"}, nil
					default:
						return map[string]any{}, nil
					}
				}
				done := make(chan *tools.ToolCallResult, 1)
				go func() {
					result, err := ts.run(ctx, shell.RunShellArgs{Cmd: "command"}, tools.NopRuntime{})
					assert.NoError(t, err)
					done <- result
				}()
				req := <-entered
				cancel()
				synctest.Wait()
				if lateCreate {
					select {
					case <-done:
						t.Fatal("create must remain owned until reply")
					default:
					}
					require.NoError(t, peer.reply(req.id, acpsdk.CreateTerminalResponse{TerminalId: "late"}, nil))
				}
				result := <-done
				assert.Contains(t, result.Output, "cancelled")
				assert.Contains(t, peer.methods(), "terminal/kill")
				assert.Contains(t, peer.methods(), "terminal/release")
			})
		})
	}
}

func TestClientTerminalTimeoutAndUnknownCreate(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ts, owner, ctx, peer := newTerminalFixture(t, latest.Toolset{})
		peer.respond = func(terminalRequest) (any, *acpsdk.RequestError) { return nil, nil }
		result := callTerminalTool(t, ctx, ts, `{"cmd":"command","timeout":1}`)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Output, "outcome is unknown")
		require.Error(t, owner.terminals.failure())
		<-owner.close(t.Context())
		require.Error(t, owner.cleanupErr)
		assert.Equal(t, []string{"terminal/create"}, peer.methods(), "never retry an ambiguous execution")
	})
	synctest.Test(t, func(t *testing.T) {
		ts, _, ctx, peer := newTerminalFixture(t, latest.Toolset{})
		peer.respond = func(r terminalRequest) (any, *acpsdk.RequestError) {
			switch r.method {
			case "terminal/create":
				return acpsdk.CreateTerminalResponse{TerminalId: "id"}, nil
			case "terminal/wait_for_exit":
				return nil, nil
			case "terminal/output":
				return acpsdk.TerminalOutputResponse{Output: "partial"}, nil
			default:
				return map[string]any{}, nil
			}
		}
		result := callTerminalTool(t, ctx, ts, `{"cmd":"command","timeout":1}`)
		assert.Contains(t, result.Output, "timed out after 1s")
		assert.Contains(t, peer.methods(), "terminal/kill")
		assert.Contains(t, peer.methods(), "terminal/release")
	})
}

func TestClientTerminalReleaseRetriesOnlyDuringFinalCleanup(t *testing.T) {
	t.Parallel()
	for _, persistent := range []bool{false, true} {
		ts, owner, ctx, peer := newTerminalFixture(t, latest.Toolset{})
		releases := 0
		peer.respond = func(r terminalRequest) (any, *acpsdk.RequestError) {
			switch r.method {
			case "terminal/create":
				return acpsdk.CreateTerminalResponse{TerminalId: "id"}, nil
			case "terminal/wait_for_exit":
				return acpsdk.WaitForTerminalExitResponse{ExitCode: new(0)}, nil
			case "terminal/output":
				return acpsdk.TerminalOutputResponse{Output: "ran"}, nil
			case "terminal/release":
				releases++
				if persistent || releases == 1 {
					return nil, acpsdk.NewInternalError("release failed")
				}
			}
			return map[string]any{}, nil
		}
		result := callTerminalTool(t, ctx, ts, `{"cmd":"command"}`)
		assert.True(t, result.IsError)
		require.Error(t, owner.terminals.failure())
		again := callTerminalTool(t, ctx, ts, `{"cmd":"must not run"}`)
		assert.True(t, again.IsError)
		assert.Equal(t, 1, releases)
		<-owner.close(t.Context())
		err := owner.cleanupErr
		if persistent {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
		assert.Equal(t, 2, releases)
		<-owner.close(t.Context())
		assert.Equal(t, 2, releases)
	}
}

func TestClientTerminalConcurrentOwnershipAndClose(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ts, owner, ctx, peer := newTerminalFixture(t, latest.Toolset{})
		entered := make(chan struct{}, 2)
		next := 0
		peer.respond = func(r terminalRequest) (any, *acpsdk.RequestError) {
			switch r.method {
			case "terminal/create":
				next++
				return acpsdk.CreateTerminalResponse{TerminalId: fmt.Sprintf("id-%d", next)}, nil
			case "terminal/wait_for_exit":
				entered <- struct{}{}
				return nil, nil
			case "terminal/output":
				return acpsdk.TerminalOutputResponse{}, nil
			default:
				return map[string]any{}, nil
			}
		}
		var wg sync.WaitGroup
		for range 2 {
			wg.Go(func() {
				_, err := ts.run(ctx, shell.RunShellArgs{Cmd: "command"}, tools.NopRuntime{})
				assert.NoError(t, err)
			})
		}
		<-entered
		<-entered
		owner.close(t.Context())
		wg.Wait()
		<-owner.close(t.Context())
		require.NoError(t, owner.cleanupErr)
		assert.Equal(t, 2, strings.Count(strings.Join(peer.methods(), ","), "terminal/release"))
		result := callTerminalTool(t, ctx, ts, `{"cmd":"stale handler"}`)
		assert.True(t, result.IsError)
	})
}

func TestClientTerminalValidationBeforeCreate(t *testing.T) {
	t.Parallel()
	ts, _, base, peer := newTerminalFixture(t, latest.Toolset{})
	ctx, cancel := context.WithCancel(base)
	cancel()
	result := callTerminalTool(t, ctx, ts, `{"cmd":"never"}`)
	assert.True(t, result.IsError)
	assert.Empty(t, peer.snapshot())
	result = callTerminalTool(t, t.Context(), ts, `{"cmd":"no owner"}`)
	assert.True(t, result.IsError)
	_, err := newTerminalToolset(t.Context(), latest.Toolset{Env: map[string]string{"bad=name": "value"}}, &config.RuntimeConfig{})
	require.Error(t, err)
}

func connectTerminalPeer(t *testing.T, a *Agent) *terminalPeer {
	t.Helper()
	reader, writer := io.Pipe()
	peer := &terminalPeer{peer: writer}
	conn := a.NewConnection(peer, reader)
	conn.SetLogger(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = writer.Close(); <-conn.Done() })
	return peer
}

func terminalAgent(t *testing.T, capability, readonly bool, cleanupFailure ...bool) (*Agent, *Session, *terminalPeer) {
	t.Helper()
	options := ""
	if readonly {
		options = "        readonly: true\n"
	}
	cfg := "agents:\n  root:\n    model: openai/gpt-4o\n    toolsets:\n      - type: shell\n" + options + "      - type: environment\n"
	a := NewAgent(config.NewBytesSource("agent.yaml", []byte(cfg)), &config.RuntimeConfig{EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test-key"})}, session.NewInMemorySessionStore())
	peer := connectTerminalPeer(t, a)
	t.Cleanup(func() {
		err := a.Stop(t.Context())
		if len(cleanupFailure) > 0 && cleanupFailure[0] {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
	})
	_, err := a.Initialize(t.Context(), acpsdk.InitializeRequest{ClientCapabilities: acpsdk.ClientCapabilities{Terminal: capability}})
	require.NoError(t, err)
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	return a, a.sessions[string(created.SessionId)], peer
}

func TestClientTerminalCapabilityRegistryAndReadonly(t *testing.T) {
	t.Parallel()
	for _, supported := range []bool{false, true} {
		for _, readonly := range []bool{false, true} {
			a, s, peer := terminalAgent(t, supported, readonly)
			available, err := s.rt.CurrentAgentTools(t.Context())
			require.NoError(t, err)
			var shellTool *tools.Tool
			for _, tool := range available {
				if tool.Name == "shell" {
					shellTool = &tool
				}
			}
			if readonly {
				assert.Nil(t, shellTool)
				continue
			}
			require.NotNil(t, shellTool)
			assert.Empty(t, peer.snapshot(), "tool discovery must not create terminals")
			if supported {
				assert.Contains(t, shellTool.Description, "ACP client")
				result, err := shellTool.Handler(context.WithValue(t.Context(), terminalOwnerKey{}, s.terminals), tools.ToolCall{Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"echo client"}`}}, tools.NopRuntime{})
				require.NoError(t, err)
				require.False(t, result.IsError, result.Output)
				assert.Equal(t, "output", result.Output)
				assert.Equal(t, "terminal/create", peer.methods()[0])
				var req acpsdk.CreateTerminalRequest
				require.NoError(t, json.Unmarshal(peer.snapshot()[0].params, &req))
				assert.Equal(t, acpsdk.SessionId(s.id), req.SessionId)
			} else {
				result, err := shellTool.Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"echo native"}`}}, tools.NopRuntime{})
				require.NoError(t, err)
				assert.Contains(t, result.Output, "native")
				assert.Empty(t, peer.snapshot())
			}
			require.NoError(t, a.Stop(t.Context()))
		}
	}
}

func TestClientTerminalRealRuntimeTransformsOutputAndKeepsApproval(t *testing.T) {
	t.Parallel()
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "denied", true: "allowed"}[allow], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a := NewAgent(nil, &config.RuntimeConfig{}, session.NewInMemorySessionStore())
				a.team = team.New()
				a.clientTerminal = true
				a.loadTeam = func(ctx context.Context, wd string) (*teamloader.LoadResult, error) {
					ts, err := createToolsetRegistry(a).CreateTool(ctx, latest.Toolset{Type: "shell"}, wd, &config.RuntimeConfig{Config: config.Config{WorkingDir: wd}}, "root")
					require.NoError(t, err)
					stream := &outcomeSequenceProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "terminal")}, streams: []chat.MessageStream{
						usageStream(&tools.ToolCall{ID: "shell", Type: "function", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"echo secret"}`}}, 1, 1), usageStream(nil, 1, 1),
					}}
					root := agent.New("root", "test", agent.WithModel(stream), agent.WithToolSets(ts))
					return &teamloader.LoadResult{Team: team.New(team.WithAgents(root))}, nil
				}
				peer := connectTerminalPeer(t, a)
				// Permission and terminal requests share the same connection and responder pipe.
				peer.other = &peerResponder{t: t, out: &peer.output, peer: peer.peer, respond: func(acpsdk.RequestPermissionRequest) any {
					synctest.Wait()
					if allow {
						return permissionSelected("allow")
					}
					return permissionSelected("reject")
				}}
				defer func() { require.NoError(t, a.Stop(t.Context())) }()
				created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
				require.NoError(t, err)
				s := a.sessions[string(created.SessionId)]
				s.sess.SetSafetyPolicy(session.SafetyPolicyStrict)
				// Install a rewrite hook on a real runtime while retaining the session-owned terminal manager.
				registry := hooks.NewRegistry()
				require.NoError(t, registry.RegisterBuiltin("rewrite", func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
					return &hooks.Output{HookSpecificOutput: &hooks.HookSpecificOutput{UpdatedToolResponse: new("transformed")}}, nil
				}))
				root, err := s.team.Agent("root")
				require.NoError(t, err)
				agent.WithHooks(&latest.HooksConfig{ToolResponseTransform: []latest.HookMatcherConfig{{Hooks: []latest.HookDefinition{{Type: "builtin", Command: "rewrite"}}}}})(root)
				require.NoError(t, s.rt.Close())
				rt, err := runtime.New(t.Context(), s.team, runtime.WithSessionCompaction(false), runtime.WithSessionStore(a.sessionStore), runtime.WithHooksRegistry(registry))
				require.NoError(t, err)
				s.rt = rt
				response, err := a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("execute")}})
				require.NoError(t, err)
				assert.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
				if allow {
					assert.Len(t, peer.snapshot(), 4)
					assert.Contains(t, strings.Join(peer.output.lines(), "\n"), "transformed")
					assert.NotContains(t, strings.Join(peer.output.lines(), "\n"), `"text":"output"`)
				} else {
					assert.Empty(t, peer.snapshot(), "rejected tools must not reach terminal/create")
				}
			})
		})
	}
}

func TestClientTerminalSessionCloseAndDeleteRetainCleanupFailure(t *testing.T) {
	t.Parallel()
	a, s, peer := terminalAgent(t, true, false, true)
	peer.respond = func(r terminalRequest) (any, *acpsdk.RequestError) {
		switch r.method {
		case "terminal/create":
			return acpsdk.CreateTerminalResponse{TerminalId: "id"}, nil
		case "terminal/wait_for_exit":
			return acpsdk.WaitForTerminalExitResponse{ExitCode: new(0)}, nil
		case "terminal/output":
			return acpsdk.TerminalOutputResponse{}, nil
		default:
			return nil, acpsdk.NewInternalError("release failed")
		}
	}
	available, err := s.rt.CurrentAgentTools(t.Context())
	require.NoError(t, err)
	for _, tool := range available {
		if tool.Name == "shell" {
			result, err := tool.Handler(context.WithValue(t.Context(), terminalOwnerKey{}, s.terminals), tools.ToolCall{Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"command"}`}}, tools.NopRuntime{})
			require.NoError(t, err)
			assert.True(t, result.IsError)
		}
	}
	_, err = a.UnstableDeleteSession(t.Context(), acpsdk.UnstableDeleteSessionRequest{SessionId: acpsdk.SessionId(s.id)})
	require.Error(t, err)
	_, err = a.sessionStore.GetSession(t.Context(), s.id)
	require.NoError(t, err)
	require.Error(t, a.Stop(t.Context()))
}

func TestClientTerminalQueuedPromptRechecksCleanupHealth(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := &Session{terminals: newTerminalManager(nil, "sid")}
		_, finish, err := s.startTurn(t.Context())
		require.NoError(t, err)
		queued := make(chan error, 1)
		go func() {
			_, finish, err := s.startTurn(t.Context())
			if finish != nil {
				finish()
			}
			queued <- err
		}()
		synctest.Wait()
		s.terminals.mu.Lock()
		s.terminals.unreleased["id"] = errors.New("release failed while queued")
		s.terminals.mu.Unlock()
		finish()
		require.ErrorContains(t, <-queued, "cleanup is unresolved")
	})
}

func TestClientTerminalDefinitiveRejectionDoesNotPoisonOwner(t *testing.T) {
	t.Parallel()
	for _, code := range []int{-32600, -32601, -32602} {
		ts, owner, ctx, peer := newTerminalFixture(t, latest.Toolset{})
		peer.respond = func(terminalRequest) (any, *acpsdk.RequestError) {
			return nil, &acpsdk.RequestError{Code: code, Message: "rejected"}
		}
		result := callTerminalTool(t, ctx, ts, `{"cmd":"command"}`)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Output, "creation rejected")
		require.NoError(t, owner.terminals.failure())
		peer.respond = nil
		result = callTerminalTool(t, ctx, ts, `{"cmd":"retry"}`)
		assert.False(t, result.IsError, result.Output)
		<-owner.close(t.Context())
		require.NoError(t, owner.cleanupErr)
	}
}

func TestClientTerminalInterpreterIsAbsolute(t *testing.T) {
	t.Parallel()
	command, args := terminalInterpreter()
	require.NotEmpty(t, command)
	assert.True(t, filepath.IsAbs(command), "client execution must not search cwd/PATH for an interpreter")
	assert.NotEmpty(t, args)
}

func TestClientTerminalCloseJoinsLateCreateAndBackgroundWait(t *testing.T) {
	t.Parallel()
	for _, lateCreate := range []bool{false, true} {
		t.Run(map[bool]string{false: "background wait", true: "late create"}[lateCreate], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, s := newResumeFixture(t, t.TempDir())
				ts, _, ctx, peer := terminalFixtureForSession(t, latest.Toolset{}, s)
				entered := make(chan terminalRequest, 1)
				peer.respond = func(r terminalRequest) (any, *acpsdk.RequestError) {
					switch r.method {
					case "terminal/create":
						if lateCreate {
							entered <- r
							return nil, nil
						}
						return acpsdk.CreateTerminalResponse{TerminalId: "id"}, nil
					case "terminal/wait_for_exit":
						entered <- r
						return nil, nil
					case "terminal/output":
						return acpsdk.TerminalOutputResponse{}, nil
					default:
						return map[string]any{}, nil
					}
				}
				done := make(chan *tools.ToolCallResult, 1)
				go func() {
					result, err := ts.run(context.WithoutCancel(ctx), shell.RunShellArgs{Cmd: "command"}, tools.NopRuntime{})
					assert.NoError(t, err)
					done <- result
				}()
				request := <-entered
				closed := closeSessionAsync(a, t.Context(), s.id)
				synctest.Wait()
				if lateCreate {
					assertPending(t, closed)
					require.NoError(t, peer.reply(request.id, acpsdk.CreateTerminalResponse{TerminalId: "id"}, nil))
				}
				assert.Contains(t, (<-done).Output, "cancelled")
				require.NoError(t, <-closed)
				assert.Equal(t, "terminal/release", peer.methods()[len(peer.methods())-1])
				assert.Equal(t, 1, strings.Count(strings.Join(peer.methods(), ","), "terminal/release"))
				before := len(peer.snapshot())
				result := callTerminalTool(t, ctx, ts, `{"cmd":"stale owner"}`)
				assert.True(t, result.IsError)
				assert.Len(t, peer.snapshot(), before)
			})
		})
	}
}

func TestClientTerminalWaitErrorAndMissingCreateID(t *testing.T) {
	t.Parallel()
	for _, missingID := range []bool{false, true} {
		ts, owner, ctx, peer := newTerminalFixture(t, latest.Toolset{})
		peer.respond = func(r terminalRequest) (any, *acpsdk.RequestError) {
			switch r.method {
			case "terminal/create":
				if missingID {
					return acpsdk.CreateTerminalResponse{}, nil
				}
				return acpsdk.CreateTerminalResponse{TerminalId: "id"}, nil
			case "terminal/wait_for_exit":
				return nil, acpsdk.NewInternalError("wait failed")
			case "terminal/output":
				return acpsdk.TerminalOutputResponse{Output: "partial"}, nil
			default:
				return map[string]any{}, nil
			}
		}
		result := callTerminalTool(t, ctx, ts, `{"cmd":"command"}`)
		assert.True(t, result.IsError)
		if missingID {
			require.Error(t, owner.terminals.failure())
			assert.Equal(t, []string{"terminal/create"}, peer.methods())
		} else {
			assert.Contains(t, result.Output, "Error waiting")
			assert.Equal(t, []string{"terminal/create", "terminal/wait_for_exit", "terminal/kill", "terminal/output", "terminal/release"}, peer.methods())
			require.NoError(t, owner.terminals.failure())
		}
	}
}

func TestClientTerminalSessionIsolationAndDisconnectNoFallback(t *testing.T) {
	t.Parallel()
	a, first, peer := terminalAgent(t, true, false)
	second, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	for _, s := range []*Session{first, a.sessions[string(second.SessionId)]} {
		available, err := s.rt.CurrentAgentTools(t.Context())
		require.NoError(t, err)
		for _, tool := range available {
			if tool.Name != "shell" {
				continue
			}
			result, err := tool.Handler(context.WithValue(t.Context(), terminalOwnerKey{}, s.terminals), tools.ToolCall{Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"echo remote"}`}}, tools.NopRuntime{})
			require.NoError(t, err)
			require.False(t, result.IsError, result.Output)
		}
	}
	var ids []acpsdk.SessionId
	for _, request := range peer.snapshot() {
		if request.method == "terminal/create" {
			var params acpsdk.CreateTerminalRequest
			require.NoError(t, json.Unmarshal(request.params, &params))
			ids = append(ids, params.SessionId)
		}
	}
	assert.Equal(t, []acpsdk.SessionId{acpsdk.SessionId(first.id), second.SessionId}, ids)
	// A missing connection is a client execution error, not a reason to run locally.
	ts, owner, ctx, _ := newTerminalFixture(t, latest.Toolset{})
	owner.terminals.conn = nil
	result := callTerminalTool(t, ctx, ts, `{"cmd":"echo must-not-run-locally"}`)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "connection unavailable")
}

func TestClientTerminalCompletedCommandNotTimedOutBySlowOutput(t *testing.T) {
	t.Parallel()
	for _, code := range []int{0, 7} {
		synctest.Test(t, func(t *testing.T) {
			ts, _, ctx, peer := newTerminalFixture(t, latest.Toolset{})
			peer.respond = func(r terminalRequest) (any, *acpsdk.RequestError) {
				switch r.method {
				case "terminal/create":
					return acpsdk.CreateTerminalResponse{TerminalId: "id"}, nil
				case "terminal/wait_for_exit":
					return acpsdk.WaitForTerminalExitResponse{ExitCode: &code}, nil
				case "terminal/output":
					time.Sleep(2 * time.Second) //nolint:forbidigo // synctest fake time models slow output retrieval
					return acpsdk.TerminalOutputResponse{Output: "already finished"}, nil
				default:
					return map[string]any{}, nil
				}
			}
			result := callTerminalTool(t, ctx, ts, `{"cmd":"command","timeout":1}`)
			assert.NotContains(t, result.Output, "timed out")
			assert.Contains(t, result.Output, "already finished")
			if code != 0 {
				assert.Contains(t, result.Output, "exit status 7")
			}
		})
	}
}

func TestClientTerminalCancellationDoesNotLeakUntransformedOutput(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a := NewAgent(nil, &config.RuntimeConfig{}, session.NewInMemorySessionStore())
		a.team = team.New()
		a.clientTerminal = true
		a.loadTeam = func(ctx context.Context, wd string) (*teamloader.LoadResult, error) {
			ts, err := newTerminalToolset(ctx, latest.Toolset{}, &config.RuntimeConfig{Config: config.Config{WorkingDir: wd}})
			require.NoError(t, err)
			prov := &outcomeSequenceProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "cancel-terminal")}, streams: []chat.MessageStream{
				usageStream(&tools.ToolCall{ID: "call", Type: "function", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"command"}`}}, 1, 1),
			}}
			root := agent.New("root", "test", agent.WithModel(prov), agent.WithToolSets(ts), agent.WithHooks(&latest.HooksConfig{
				ToolResponseTransform: []latest.HookMatcherConfig{{Hooks: []latest.HookDefinition{{Type: "builtin", Command: "redact_secrets"}}}},
			}))
			return &teamloader.LoadResult{Team: team.New(team.WithAgents(root))}, nil
		}
		entered := make(chan struct{})
		peer := connectTerminalPeer(t, a)
		secret := portcullistest.FakeGitHubPAT("cxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")
		peer.respond = func(r terminalRequest) (any, *acpsdk.RequestError) {
			switch r.method {
			case "terminal/create":
				return acpsdk.CreateTerminalResponse{TerminalId: "id"}, nil
			case "terminal/wait_for_exit":
				close(entered)
				return nil, nil
			case "terminal/output":
				return acpsdk.TerminalOutputResponse{Output: secret}, nil
			default:
				return map[string]any{}, nil
			}
		}
		defer func() { require.NoError(t, a.Stop(t.Context())) }()
		created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
		require.NoError(t, err)
		s := a.sessions[string(created.SessionId)]
		s.sess.SetToolsApproved(true)
		done := make(chan error, 1)
		go func() {
			response, err := a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("work")}})
			assert.Equal(t, acpsdk.StopReasonCancelled, response.StopReason)
			done <- err
		}()
		<-entered
		require.NoError(t, a.Cancel(t.Context(), acpsdk.CancelNotification{SessionId: created.SessionId}))
		require.NoError(t, <-done)
		wire := strings.Join(peer.output.lines(), "\n")
		assert.NotContains(t, wire, secret)
		history, err := json.Marshal(s.sess)
		require.NoError(t, err)
		assert.NotContains(t, string(history), secret)
		assert.Contains(t, string(history), "canceled by the user")
		assert.Contains(t, peer.methods(), "terminal/release")
	})
}

func TestClientTerminalCancellationDuringRedaction(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a := NewAgent(nil, &config.RuntimeConfig{}, session.NewInMemorySessionStore())
		a.team = team.New()
		a.clientTerminal = true
		a.loadTeam = func(ctx context.Context, wd string) (*teamloader.LoadResult, error) {
			ts, err := newTerminalToolset(ctx, latest.Toolset{}, &config.RuntimeConfig{Config: config.Config{WorkingDir: wd}})
			require.NoError(t, err)
			prov := &outcomeSequenceProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "cancel-terminal")}, streams: []chat.MessageStream{
				usageStream(&tools.ToolCall{ID: "call", Type: "function", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"command"}`}}, 1, 1),
			}}
			root := agent.New("root", "test", agent.WithModel(prov), agent.WithToolSets(ts), agent.WithHooks(&latest.HooksConfig{
				ToolResponseTransform: []latest.HookMatcherConfig{{Hooks: []latest.HookDefinition{{Type: "builtin", Command: "redact_secrets"}}}},
			}))
			return &teamloader.LoadResult{Team: team.New(team.WithAgents(root))}, nil
		}
		entered := make(chan struct{})
		peer := connectTerminalPeer(t, a)
		secret := portcullistest.FakeGitHubPAT("cxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")
		peer.respond = func(r terminalRequest) (any, *acpsdk.RequestError) {
			switch r.method {
			case "terminal/create":
				return acpsdk.CreateTerminalResponse{TerminalId: "id"}, nil
			case "terminal/wait_for_exit":
				return acpsdk.WaitForTerminalExitResponse{ExitCode: new(0)}, nil
			case "terminal/output":
				return acpsdk.TerminalOutputResponse{Output: secret}, nil
			default:
				return map[string]any{}, nil
			}
		}
		defer func() { require.NoError(t, a.Stop(t.Context())) }()
		created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
		require.NoError(t, err)
		s := a.sessions[string(created.SessionId)]
		s.sess.SetToolsApproved(true)
		// Stall the real redaction hook to cancel at its execution boundary.
		registry := hooks.NewRegistry()
		require.NoError(t, s.rt.Close())
		s.rt, err = runtime.New(t.Context(), s.team, runtime.WithSessionCompaction(false), runtime.WithSessionStore(a.sessionStore), runtime.WithHooksRegistry(registry))
		require.NoError(t, err)
		redact, ok := registry.LookupBuiltin("redact_secrets")
		require.True(t, ok)
		require.NoError(t, registry.RegisterBuiltin("redact_secrets", func(ctx context.Context, input *hooks.Input, args []string) (*hooks.Output, error) {
			close(entered)
			<-ctx.Done()
			return redact(ctx, input, args)
		}))
		done := make(chan error, 1)
		go func() {
			response, err := a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("work")}})
			assert.Equal(t, acpsdk.StopReasonCancelled, response.StopReason)
			done <- err
		}()
		<-entered
		require.NoError(t, a.Cancel(t.Context(), acpsdk.CancelNotification{SessionId: created.SessionId}))
		require.NoError(t, <-done)
		wire := strings.Join(peer.output.lines(), "\n")
		assert.NotContains(t, wire, secret, "raw PAT leaked to ACP wire after cancellation during redaction")
		history, err := json.Marshal(s.sess)
		require.NoError(t, err)
		assert.NotContains(t, string(history), secret, "raw PAT leaked to stored history after cancellation during redaction")

		assert.Contains(t, peer.methods(), "terminal/release")
	})
}
