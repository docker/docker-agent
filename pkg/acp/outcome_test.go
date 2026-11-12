package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func stoppedEvent(sid, reason string, finish chat.FinishReason) runtime.Event {
	return &runtime.StreamStoppedEvent{SessionID: sid, Reason: reason, FinishReason: finish}
}

func TestPromptOutcomeUsesOnlyRootSession(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		events func() []runtime.Event
		stop   acpsdk.StopReason
		code   string
	}{
		{name: "normal", stop: acpsdk.StopReasonEndTurn, events: func() []runtime.Event {
			return []runtime.Event{stoppedEvent(testSessionID, "normal", chat.FinishReasonStop)}
		}},
		{name: "length", stop: acpsdk.StopReasonMaxTokens, events: func() []runtime.Event {
			return []runtime.Event{stoppedEvent(testSessionID, "normal", chat.FinishReasonLength)}
		}},
		{name: "refusal", stop: acpsdk.StopReasonRefusal, events: func() []runtime.Event {
			return []runtime.Event{stoppedEvent(testSessionID, "normal", chat.FinishReasonRefusal)}
		}},
		{name: "iteration limit", stop: acpsdk.StopReasonMaxTurnRequests, events: func() []runtime.Event {
			return []runtime.Event{stoppedEvent(testSessionID, runtime.StreamStopReasonMaxIterations, "")}
		}},
		{name: "runtime cancellation", stop: acpsdk.StopReasonCancelled, events: func() []runtime.Event { return []runtime.Event{stoppedEvent(testSessionID, "canceled", "")} }},
		{name: "fatal before model", code: runtime.ErrorCodeModelError, events: func() []runtime.Event {
			return []runtime.Event{runtime.ErrorWithCodeForSession(testSessionID, runtime.ErrorCodeModelError, "provider failed")}
		}},
		{name: "fatal after partial output", code: runtime.ErrorCodeRateLimited, events: func() []runtime.Event {
			return []runtime.Event{runtime.AgentChoice("root", testSessionID, "partial"), runtime.ErrorWithCodeForSession(testSessionID, runtime.ErrorCodeRateLimited, "provider failed"), stoppedEvent(testSessionID, "error", "")}
		}},
		{name: "terminal error without details", code: "error", events: func() []runtime.Event { return []runtime.Event{stoppedEvent(testSessionID, "error", "")} }},
		{name: "budget is not request limit", code: "budget_exceeded", events: func() []runtime.Event { return []runtime.Event{stoppedEvent(testSessionID, "budget_exceeded", "")} }},
		{name: "recoverable diagnostics", stop: acpsdk.StopReasonEndTurn, events: func() []runtime.Event {
			return []runtime.Event{runtime.ErrorForSession(testSessionID, "compaction failed"), runtime.Error("RAG diagnostic"), runtime.ErrorWithCodeForSession(testSessionID, "future_diagnostic", "nonterminal"), stoppedEvent(testSessionID, "normal", chat.FinishReasonStop)}
		}},
		{name: "child errors and limits", stop: acpsdk.StopReasonEndTurn, events: func() []runtime.Event {
			return []runtime.Event{runtime.ErrorWithCodeForSession("child", runtime.ErrorCodeModelError, "child failed"), stoppedEvent("child", "error", chat.FinishReasonRefusal), stoppedEvent("child", runtime.StreamStopReasonMaxIterations, chat.FinishReasonLength), stoppedEvent(testSessionID, "normal", chat.FinishReasonStop)}
		}},
		{name: "later successful result wins", stop: acpsdk.StopReasonEndTurn, events: func() []runtime.Event {
			return []runtime.Event{runtime.MessageAdded(testSessionID, &session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, FinishReason: chat.FinishReasonLength}}, "root"), stoppedEvent(testSessionID, "normal", chat.FinishReasonStop)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				a, _, _ := newPromptTestAgent(t, &fakeRuntime{events: tc.events()})
				response, err := a.Prompt(t.Context(), promptRequest("test"))
				if tc.code != "" {
					var rpcErr *acpsdk.RequestError
					require.ErrorAs(t, err, &rpcErr)
					assert.Equal(t, -32603, rpcErr.Code)
					data := rpcErr.Data.(map[string]any)
					assert.Equal(t, tc.code, data["runtimeCode"])
					assert.Equal(t, testSessionID, data["sessionId"])
					assert.Empty(t, response.StopReason)
				} else {
					require.NoError(t, err)
					assert.Equal(t, tc.stop, response.StopReason)
				}
			})
		})
	}
}

func TestPromptModelFinishReasons(t *testing.T) {
	t.Parallel()
	for _, finish := range []chat.FinishReason{chat.FinishReasonStop, chat.FinishReasonLength, chat.FinishReasonRefusal} {
		for _, text := range []string{"", "reply"} {
			for _, withUsage := range []bool{false, true} {
				t.Run(string(finish)+"/"+text+"/"+strconv.FormatBool(withUsage), func(t *testing.T) {
					t.Parallel()
					var usage *chat.Usage
					if withUsage {
						usage = &chat.Usage{InputTokens: 2, OutputTokens: 1}
					}
					stream := &mockStream{responses: []chat.MessageStreamResponse{
						{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: text}}}},
						{Choices: []chat.MessageStreamChoice{{FinishReason: finish}}, Usage: usage},
					}}
					prov := &mockProvider{id: modelsdev.NewID("test", "finish"), stream: stream}
					rt, err := runtime.New(t.Context(), team.New(team.WithAgents(agent.New("root", "test", agent.WithModel(prov)))), runtime.WithSessionCompaction(false))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, rt.Close()) })
					a, _, _ := newPromptTestAgent(t, rt)
					response, err := a.Prompt(t.Context(), promptRequest("test"))
					require.NoError(t, err)
					want := map[chat.FinishReason]acpsdk.StopReason{chat.FinishReasonStop: acpsdk.StopReasonEndTurn, chat.FinishReasonLength: acpsdk.StopReasonMaxTokens, chat.FinishReasonRefusal: acpsdk.StopReasonRefusal}[finish]
					assert.Equal(t, want, response.StopReason)
				})
			}
		}
	}
}

type outcomeDrainRuntime struct {
	fakeRuntime

	sent    chan struct{}
	release chan struct{}
	fatal   bool
}

func (r *outcomeDrainRuntime) RunStream(ctx context.Context, sess *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event)
	go func() {
		defer close(ch)
		if r.fatal {
			ch <- runtime.ErrorWithCodeForSession(sess.ID, runtime.ErrorCodeModelError, "failed")
		}
		ch <- stoppedEvent(sess.ID, "normal", chat.FinishReasonLength)
		close(r.sent)
		<-r.release
	}()
	return ch
}

func TestPromptOutcomeWaitsForDrainAndCancellationWins(t *testing.T) {
	t.Parallel()
	for _, cancelTurn := range []bool{false, true} {
		for _, fatal := range []bool{false, true} {
			t.Run(strconv.FormatBool(cancelTurn)+"/"+strconv.FormatBool(fatal), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					rt := &outcomeDrainRuntime{sent: make(chan struct{}), release: make(chan struct{}), fatal: fatal}
					a, _, _ := newPromptTestAgent(t, rt)
					done := promptAsync(a, t.Context(), promptRequest("test"))
					<-rt.sent
					if cancelTurn {
						require.NoError(t, a.Cancel(t.Context(), acpsdk.CancelNotification{SessionId: testSessionID}))
					}
					synctest.Wait()
					select {
					case <-done:
						t.Fatal("prompt returned before runtime drain")
					default:
					}
					close(rt.release)
					result := <-done
					switch {
					case cancelTurn:
						require.NoError(t, result.err)
						assert.Equal(t, acpsdk.StopReasonCancelled, result.response.StopReason)
					case fatal:
						require.Error(t, result.err)
					default:
						require.NoError(t, result.err)
						assert.Equal(t, acpsdk.StopReasonMaxTokens, result.response.StopReason)
					}
				})
			})
		}
	}
}

func TestPromptStructuredErrorsOnWire(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		a, s := newResumeFixture(t, t.TempDir())
		s.rt = &fakeRuntime{events: []runtime.Event{runtime.ErrorWithCodeForSession(s.sess.ID, runtime.ErrorCodeRateLimited, "provider rate limited")}}
		reader, send := io.Pipe()
		receive, writer := io.Pipe()
		conn := acpsdk.NewAgentSideConnection(a, writer, reader)
		a.SetAgentConnection(conn)
		t.Cleanup(func() { _ = send.Close(); _ = receive.Close(); <-conn.Done() })
		encoder, decoder := json.NewEncoder(send), json.NewDecoder(receive)
		for i, tc := range []struct {
			method      string
			params      map[string]any
			code        int
			runtimeCode string
		}{
			{"session/prompt", map[string]any{"sessionId": "missing", "prompt": []any{}}, -32002, ""},
			{"session/resume", map[string]any{"sessionId": "missing", "cwd": s.workingDir}, -32002, ""},
			{"session/new", map[string]any{"cwd": filepath.Join(t.TempDir(), "missing"), "mcpServers": []any{}}, -32602, ""},
			{"session/new", map[string]any{"cwd": s.workingDir, "additionalDirectories": []string{"relative"}, "mcpServers": []any{}}, -32602, ""},
			{"session/prompt", map[string]any{"sessionId": s.id, "prompt": []any{map[string]any{"type": "text", "text": "hello"}}}, -32603, runtime.ErrorCodeRateLimited},
		} {
			require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": i, "method": tc.method, "params": tc.params}))
			for {
				var response struct {
					ID     *int                 `json:"id"`
					Error  *acpsdk.RequestError `json:"error"`
					Method string               `json:"method"`
				}
				require.NoError(t, decoder.Decode(&response))
				if response.ID == nil {
					assert.Equal(t, "session/update", response.Method)
					continue
				}
				assert.Equal(t, i, *response.ID)
				require.NotNil(t, response.Error)
				assert.Equal(t, tc.code, response.Error.Code)
				if tc.runtimeCode != "" {
					data := response.Error.Data.(map[string]any)
					assert.Equal(t, tc.runtimeCode, data["runtimeCode"])
					assert.Equal(t, s.id, data["sessionId"])
					assert.Equal(t, "provider rate limited", data["error"])
				}
				break
			}
		}
	})
}

type failureProvider struct{ mockProvider }

func (*failureProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, errors.New("provider unavailable")
}

func TestPromptFatalModelError(t *testing.T) {
	t.Parallel()
	prov := &failureProvider{mockProvider{id: modelsdev.NewID("test", "failure")}}
	rt, err := runtime.New(t.Context(), team.New(team.WithAgents(agent.New("root", "test", agent.WithModel(prov)))), runtime.WithSessionCompaction(false))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	a, _, peer := newPromptTestAgent(t, rt)
	_, err = a.Prompt(t.Context(), promptRequest("test"))
	var rpcErr *acpsdk.RequestError
	require.ErrorAs(t, err, &rpcErr)
	assert.Equal(t, runtime.ErrorCodeModelError, rpcErr.Data.(map[string]any)["runtimeCode"])
	assert.Contains(t, rpcErr.Data.(map[string]any)["error"], "provider unavailable")
	out := peer.out.(*captureWriter)
	assert.Contains(t, strings.Join(out.lines(), "\n"), "provider unavailable")
}

func TestPromptIterationLimitOutcome(t *testing.T) {
	t.Parallel()
	for _, selection := range []string{"stop", "continue"} {
		t.Run(selection, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				prov := &elicitationTestProvider{approvalTestProvider: approvalTestProvider{
					mockProvider: mockProvider{id: modelsdev.NewID("test", "limit")}, toolNames: []string{"noop"},
				}}
				root := agent.New("root", "test", agent.WithModel(prov), agent.WithTools(tools.Tool{
					Name: "noop", Parameters: map[string]any{"type": "object"},
					Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
						return tools.ResultSuccess("done"), nil
					},
				}))
				rt, err := runtime.New(t.Context(), team.New(team.WithAgents(root)), runtime.WithSessionCompaction(false))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, rt.Close()) })
				f := newRunAgentFixtureWithPermissions(t, &fakeRuntime{}, &captureWriter{}, func(req acpsdk.RequestPermissionRequest) any {
					synctest.Wait()
					assert.Equal(t, acpsdk.ToolCallId("max_iterations"), req.ToolCall.ToolCallId)
					return permissionSelected(selection)
				})
				f.sess.rt = rt
				f.sess.sess.MaxIterations = 1
				f.sess.sess.ToolsApproved = true
				f.agent.sessions[testSessionID] = f.sess
				response, err := f.agent.Prompt(t.Context(), promptRequest("test"))
				require.NoError(t, err)
				if selection == "stop" {
					assert.Equal(t, acpsdk.StopReasonMaxTurnRequests, response.StopReason)
				} else {
					assert.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
					assert.Equal(t, "Done", f.sess.sess.GetLastAssistantMessageContent())
				}
				assert.Len(t, f.peer.recordedRequests(), 1)
			})
		})
	}
}

type outcomeSequenceProvider struct {
	mockProvider

	streams []chat.MessageStream
}

func (p *outcomeSequenceProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	stream := p.streams[0]
	p.streams = p.streams[1:]
	return stream, nil
}

func TestPromptFinalFollowupOverridesEarlierFinishReason(t *testing.T) {
	t.Parallel()
	for _, finish := range []chat.FinishReason{chat.FinishReasonLength, chat.FinishReasonRefusal} {
		t.Run(string(finish), func(t *testing.T) {
			t.Parallel()
			prov := &outcomeSequenceProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "followup")}, streams: []chat.MessageStream{
				&mockStream{responses: []chat.MessageStreamResponse{{Choices: []chat.MessageStreamChoice{{FinishReason: finish}}}}},
				&mockStream{responses: []chat.MessageStreamResponse{
					{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "final answer"}}}},
					{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}},
				}},
			}}
			rt, err := runtime.New(t.Context(), team.New(team.WithAgents(agent.New("root", "test", agent.WithModel(prov)))), runtime.WithSessionCompaction(false))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close()) })
			require.NoError(t, rt.FollowUp(t.Context(), runtime.QueuedMessage{Content: "follow up"}))
			a, s, _ := newPromptTestAgent(t, rt)
			response, err := a.Prompt(t.Context(), promptRequest("test"))
			require.NoError(t, err)
			assert.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
			assert.Equal(t, "final answer", s.sess.GetLastAssistantMessageContent())
		})
	}
}

func TestPromptOutcomeFallbackAndFatalCodes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		finish chat.FinishReason
		want   acpsdk.StopReason
	}{
		{chat.FinishReasonLength, acpsdk.StopReasonMaxTokens},
		{chat.FinishReasonRefusal, acpsdk.StopReasonRefusal},
		{"", acpsdk.StopReasonEndTurn},
		{"future", acpsdk.StopReasonEndTurn},
	} {
		o := promptOutcome{sessionID: "root"}
		o.observe(runtime.MessageAdded("root", &session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, FinishReason: tc.finish}}, "root"))
		stop, err := o.result()
		require.NoError(t, err)
		assert.Equal(t, tc.want, stop)
	}
	for _, code := range []string{runtime.ErrorCodeModelError, runtime.ErrorCodeRateLimited, runtime.ErrorCodeContextExceeded, runtime.ErrorCodeRequestTooLarge, runtime.ErrorCodeMediaTooLarge, runtime.ErrorCodeToolFailed, runtime.ErrorCodeHookBlocked, runtime.ErrorCodeLoopDetected, runtime.ErrorCodeStructuredOutputFailed} {
		o := promptOutcome{sessionID: "root"}
		o.observe(runtime.ErrorWithCodeForSession("root", code, "fatal"))
		o.observe(runtime.ErrorForSession("root", "later recoverable diagnostic"))
		_, err := o.result()
		var rpcErr *acpsdk.RequestError
		require.ErrorAs(t, err, &rpcErr)
		assert.Equal(t, code, rpcErr.Data.(map[string]any)["runtimeCode"])
		assert.Equal(t, "fatal", rpcErr.Data.(map[string]any)["error"])
	}
	for _, sid := range []string{"root", "child"} {
		o := promptOutcome{sessionID: "root"}
		o.observe(&runtime.BudgetExceededEvent{SessionID: sid, Message: "cost ceiling reached"})
		_, err := o.result()
		if sid == "root" {
			require.ErrorContains(t, err, "cost ceiling reached")
		} else {
			require.NoError(t, err)
		}
	}
}

type recoveringOutcomeProvider struct {
	mockProvider

	failed bool
}

func (p *recoveringOutcomeProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	if !p.failed {
		p.failed = true
		return nil, modelerrors.NewContextOverflowError(errors.New("context overflow"))
	}
	return &mockStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "recovered"}}}},
		{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}},
	}}, nil
}

func TestPromptCompactionDiagnosticDoesNotOverrideRecovery(t *testing.T) {
	t.Parallel()
	prov := &recoveringOutcomeProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "recovery")}}
	rt, err := runtime.New(t.Context(), team.New(team.WithAgents(agent.New("root", "test", agent.WithModel(prov)))), runtime.WithSessionCompaction(true))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	a, _, peer := newPromptTestAgent(t, rt)
	response, err := a.Prompt(t.Context(), promptRequest("test"))
	require.NoError(t, err)
	assert.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
	assert.Contains(t, strings.Join(peer.out.(*captureWriter).lines(), "\n"), "Failed to get model definition")
}

func TestPromptNaturalStopAtIterationCap(t *testing.T) {
	t.Parallel()
	prov := &mockProvider{id: modelsdev.NewID("test", "limit"), stream: &mockStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "done"}}}},
		{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}},
	}}}
	rt, err := runtime.New(t.Context(), team.New(team.WithAgents(agent.New("root", "test", agent.WithModel(prov)))), runtime.WithSessionCompaction(false))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	a, s, peer := newPromptTestAgent(t, rt)
	s.sess.MaxIterations = 1
	response, err := a.Prompt(t.Context(), promptRequest("test"))
	require.NoError(t, err)
	assert.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
	assert.Empty(t, peer.recordedRequests())
}
