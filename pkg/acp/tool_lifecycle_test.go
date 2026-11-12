package acp

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func toolUpdates(updates []acpsdk.SessionUpdate) []acpsdk.SessionUpdate {
	var result []acpsdk.SessionUpdate
	for _, update := range updates {
		if update.ToolCall != nil || update.ToolCallUpdate != nil {
			result = append(result, update)
		}
	}
	return result
}

func TestToolLifecyclePendingRunningCompletion(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		wd := t.TempDir()
		call := tools.ToolCall{ID: "runtime-id", Function: tools.FunctionCall{Name: "edit_file", Arguments: `{"file":"before.txt"}`}}
		changed := call
		changed.Function.Arguments = `{"file":"after.txt"}`
		tool := tools.Tool{Name: "edit_file", Annotations: tools.ToolAnnotations{Title: "Edit", DestructiveHint: new(true)}}
		rt := &fakeRuntime{events: []runtime.Event{
			runtime.ToolCallConfirmation(call, tool, "root", nil),
			runtime.ToolCall(changed, tool, "root"),
			runtime.ToolCallResponse(call.ID, tool, tools.ResultSuccess("done"), "done", "root"),
		}}
		f := newRunAgentFixtureWithPermissions(t, rt, &captureWriter{}, func(acpsdk.RequestPermissionRequest) any { return permissionSelected("allow") })
		f.sess.workingDir = wd
		require.NoError(t, f.runAgent(t.Context(), f.sess))
		updates := toolUpdates(f.sessionUpdates(t))
		require.Len(t, updates, 3)
		pending, running, finished := updates[0].ToolCall, updates[1].ToolCallUpdate, updates[2].ToolCallUpdate
		require.NotNil(t, pending)
		require.NotNil(t, running)
		require.NotNil(t, finished)
		assert.Equal(t, acpsdk.ToolCallStatusPending, pending.Status)
		assert.Equal(t, acpsdk.ToolCallStatusInProgress, *running.Status)
		assert.Equal(t, acpsdk.ToolCallStatusCompleted, *finished.Status)
		assert.Equal(t, pending.ToolCallId, running.ToolCallId)
		assert.Equal(t, pending.ToolCallId, finished.ToolCallId)
		req := f.peer.recordedRequests()[0]
		assert.Equal(t, pending.ToolCallId, req.ToolCall.ToolCallId)
		assert.Equal(t, acpsdk.ToolKindEdit, pending.Kind)
		assert.Equal(t, pending.Kind, *req.ToolCall.Kind)
		assert.Equal(t, pending.Kind, *running.Kind)
		assert.Equal(t, map[string]any{"file": "before.txt"}, pending.RawInput)
		assert.Equal(t, pending.RawInput, req.ToolCall.RawInput)
		assert.Equal(t, map[string]any{"file": "after.txt"}, running.RawInput)
		assert.Equal(t, filepath.Join(wd, "after.txt"), running.Locations[0].Path)
	})
}

func TestToolLifecycleSuppressesRejectedSyntheticStart(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"reject", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				call := tools.ToolCall{ID: "nested", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"never execute"}`}}
				tool := tools.Tool{Name: "shell"}
				rt := &fakeRuntime{events: []runtime.Event{
					runtime.ToolCallConfirmation(call, tool, "root", nil),
					runtime.ToolCall(call, tool, "root"),
					runtime.ToolCallResponse(call.ID, tool, tools.ResultError("rejected"), "rejected", "root"),
				}}
				f := newRunAgentFixtureWithPermissions(t, rt, &captureWriter{}, func(acpsdk.RequestPermissionRequest) any {
					if outcome == "cancelled" {
						return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
					}
					return permissionSelected("reject")
				})
				require.NoError(t, f.runAgent(t.Context(), f.sess))
				updates := toolUpdates(f.sessionUpdates(t))
				require.Len(t, updates, 2)
				assert.Equal(t, acpsdk.ToolCallStatusPending, updates[0].ToolCall.Status)
				assert.Equal(t, acpsdk.ToolCallStatusFailed, *updates[1].ToolCallUpdate.Status)
				assert.Equal(t, updates[0].ToolCall.ToolCallId, updates[1].ToolCallUpdate.ToolCallId)
				assert.Equal(t, []runtime.ResumeRequest{{Type: runtime.ResumeTypeReject}}, rt.resumeRequests())
			})
		})
	}
}

func TestToolLifecycleScopesAndReusesRuntimeIDs(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		call := tools.ToolCall{ID: "same", Function: tools.FunctionCall{Name: "shell", Arguments: "{}"}}
		tool := tools.Tool{Name: "shell"}
		rt := &fakeRuntime{events: []runtime.Event{
			runtime.ToolCall(call, tool, "root"), runtime.ToolCall(call, tool, "worker"),
			runtime.ToolCallResponse("same", tool, tools.ResultSuccess("worker"), "worker", "worker"),
			runtime.ToolCallResponse("same", tool, tools.ResultSuccess("root"), "root", "root"),
			runtime.ToolCall(call, tool, "root"), runtime.ToolCallResponse("same", tool, tools.ResultSuccess("reused"), "reused", "root"),
		}}
		f := newRunAgentFixture(t, rt, &captureWriter{})
		require.NoError(t, f.runAgent(t.Context(), f.sess))
		updates := toolUpdates(f.sessionUpdates(t))
		require.Len(t, updates, 6)
		root, worker, reused := updates[0].ToolCall.ToolCallId, updates[1].ToolCall.ToolCallId, updates[4].ToolCall.ToolCallId
		assert.NotEqual(t, root, worker)
		assert.NotEqual(t, root, reused)
		assert.NotEqual(t, worker, reused)
		assert.Equal(t, worker, updates[2].ToolCallUpdate.ToolCallId)
		assert.Equal(t, root, updates[3].ToolCallUpdate.ToolCallId)
		assert.Equal(t, reused, updates[5].ToolCallUpdate.ToolCallId)
		require.NoError(t, f.runAgent(t.Context(), f.sess))
		all := toolUpdates(f.sessionUpdates(t))
		assert.NotEqual(t, root, all[6].ToolCall.ToolCallId)
	})
}

func TestToolLifecycleRuntimeRejectAndPolicyDeny(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"allow", "reject", "policy deny", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var executed bool
				tool := tools.Tool{Name: "change", Parameters: map[string]any{"type": "object"}, Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
					executed = true
					return tools.ResultSuccess("changed"), nil
				}}
				prov := &elicitationTestProvider{approvalTestProvider: approvalTestProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "lifecycle")}, toolNames: []string{"change"}}}
				root := agent.New("root", "test", agent.WithModel(prov))
				if mode != "unavailable" {
					agent.WithTools(tool)(root)
				}
				rt, err := runtime.New(t.Context(), team.New(team.WithAgents(root)), runtime.WithSessionCompaction(false))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, rt.Close()) })
				f := newRunAgentFixtureWithPermissions(t, &fakeRuntime{}, &captureWriter{}, func(acpsdk.RequestPermissionRequest) any { synctest.Wait(); return permissionSelected(mode) })
				f.sess.rt = rt
				f.sess.sess.SetSafetyPolicy(session.SafetyPolicyStrict)
				if mode == "policy deny" {
					f.sess.sess.Permissions = &session.PermissionsConfig{Deny: []string{"change"}}
				}
				f.agent.sessions[testSessionID] = f.sess
				response, err := f.agent.Prompt(t.Context(), promptRequest("do work"))
				require.NoError(t, err)
				assert.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
				assert.Equal(t, "Done", f.sess.sess.GetLastAssistantMessageContent())
				assert.Equal(t, mode == "allow", executed)
				updates := toolUpdates(f.sessionUpdates(t))
				switch mode {
				case "allow":
					require.Len(t, updates, 3)
					assert.Equal(t, acpsdk.ToolCallStatusInProgress, *updates[1].ToolCallUpdate.Status)
					assert.Equal(t, acpsdk.ToolCallStatusCompleted, *updates[2].ToolCallUpdate.Status)
				case "reject":
					require.Len(t, updates, 2)
					assert.Equal(t, acpsdk.ToolCallStatusFailed, *updates[1].ToolCallUpdate.Status)
				default:
					require.Len(t, updates, 1)
					assert.Equal(t, acpsdk.ToolCallStatusFailed, updates[0].ToolCall.Status)
					assert.Nil(t, updates[0].ToolCall.RawInput)
				}
			})
		})
	}
}

type lateToolResultRuntime struct {
	fakeRuntime

	started     chan struct{}
	resultReady chan struct{}
	resultSent  chan struct{}
	finish      chan struct{}
}

func (r *lateToolResultRuntime) RunStream(ctx context.Context, _ *session.Session) <-chan runtime.Event {
	events := make(chan runtime.Event)
	go func() {
		defer close(events)
		call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "shell", Arguments: "{}"}}
		events <- runtime.ToolCall(call, tools.Tool{Name: "shell"}, "root")
		close(r.started)
		<-r.resultReady
		events <- runtime.ToolCallResponse("call", tools.Tool{Name: "shell"}, tools.ResultSuccess("actual result"), "actual result", "root")
		close(r.resultSent)
		<-r.finish
	}()
	return events
}

func TestCanceledPromptPreservesLateToolResultAfterSlowDrain(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rt := &lateToolResultRuntime{started: make(chan struct{}), resultReady: make(chan struct{}), resultSent: make(chan struct{}), finish: make(chan struct{})}
		a, _, peer := newPromptTestAgent(t, rt)
		done := promptAsync(a, t.Context(), promptRequest("work"))
		<-rt.started
		synctest.Wait()
		require.NoError(t, a.Cancel(t.Context(), acpsdk.CancelNotification{SessionId: testSessionID}))
		close(rt.resultReady)
		<-rt.resultSent
		// Runtime teardown may outlive the final-notification attempt budget.
		time.Sleep(3 * time.Second) //nolint:forbidigo // fake time advances a deliberately slow teardown
		close(rt.finish)
		result := <-done
		require.NoError(t, result.err)
		assert.Equal(t, acpsdk.StopReasonCancelled, result.response.StopReason)
		out := peer.out.(*captureWriter)
		assert.Contains(t, strings.Join(out.lines(), "\n"), `"status":"completed"`)
		assert.Contains(t, strings.Join(out.lines(), "\n"), "actual result")
		assert.NotContains(t, strings.Join(out.lines(), "\n"), "interrupted")
	})
}

func TestToolCompletionSurvivesCancellationBeforeSend(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		f := newRunAgentFixture(t, &fakeRuntime{}, &captureWriter{})
		var tracker toolCallTracker
		call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "shell", Arguments: "{}"}}
		_, err := tracker.report(t.Context(), f.agent, f.sess, "root", call, tools.Tool{Name: "shell"}, acpsdk.ToolCallStatusInProgress)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err = tracker.complete(ctx, f.agent, f.sess, runtime.ToolCallResponse("call", tools.Tool{Name: "shell"}, tools.ResultSuccess("actual result"), "actual result", "root").(*runtime.ToolCallResponseEvent))
		require.NoError(t, err)
		updates := toolUpdates(f.sessionUpdates(t))
		require.Len(t, updates, 2)
		assert.Equal(t, acpsdk.ToolCallStatusCompleted, *updates[1].ToolCallUpdate.Status)
		assert.Empty(t, tracker.active)
	})
}

func TestToolCompletionWriteFailureIsNotRetried(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		out := &captureWriter{failOn: func(n int) error {
			if n == 2 {
				return io.ErrClosedPipe
			}
			return nil
		}}
		f := newRunAgentFixture(t, &fakeRuntime{}, out)
		var tracker toolCallTracker
		call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "shell", Arguments: "{}"}}
		_, err := tracker.report(t.Context(), f.agent, f.sess, "root", call, tools.Tool{Name: "shell"}, acpsdk.ToolCallStatusInProgress)
		require.NoError(t, err)
		err = tracker.complete(t.Context(), f.agent, f.sess, runtime.ToolCallResponse("call", tools.Tool{Name: "shell"}, tools.ResultSuccess("done"), "done", "root").(*runtime.ToolCallResponseEvent))
		require.Error(t, err)
		require.NoError(t, tracker.interrupt(t.Context(), f.agent, f.sess))
		assert.Equal(t, 2, out.writes)
	})
}

type interruptedToolRuntime struct {
	fakeRuntime

	started chan struct{}
	release chan struct{}
	fail    bool
}

func (r *interruptedToolRuntime) RunStream(ctx context.Context, _ *session.Session) <-chan runtime.Event {
	events := make(chan runtime.Event)
	go func() {
		defer close(events)
		call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "shell", Arguments: "{}"}}
		events <- runtime.ToolCall(call, tools.Tool{Name: "shell"}, "root")
		close(r.started)
		if r.fail {
			events <- &runtime.ToolCallResponseEvent{}
		}
		<-ctx.Done()
		<-r.release
	}()
	return events
}

func TestInterruptedToolWithoutResultClosesAfterDrain(t *testing.T) {
	t.Parallel()
	for _, failure := range []bool{false, true} {
		name := "cancellation"
		if failure {
			name = "handler error"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rt := &interruptedToolRuntime{started: make(chan struct{}), release: make(chan struct{}), fail: failure}
				a, _, peer := newPromptTestAgent(t, rt)
				done := promptAsync(a, t.Context(), promptRequest("work"))
				<-rt.started
				synctest.Wait()
				if !failure {
					require.NoError(t, a.Cancel(t.Context(), acpsdk.CancelNotification{SessionId: testSessionID}))
				}
				synctest.Wait()
				select {
				case <-done:
					t.Fatal("prompt returned before runtime teardown")
				default:
				}
				out := peer.out.(*captureWriter)
				assert.NotContains(t, strings.Join(out.lines(), "\n"), `"status":"failed"`)
				close(rt.release)
				result := <-done
				if failure {
					require.EqualError(t, result.err, "tool call ID is required")
				} else {
					require.NoError(t, result.err)
					assert.Equal(t, acpsdk.StopReasonCancelled, result.response.StopReason)
				}
				f := &runAgentFixture{out: out}
				updates := toolUpdates(f.sessionUpdates(t))
				require.Len(t, updates, 2)
				assert.Equal(t, acpsdk.ToolCallStatusInProgress, updates[0].ToolCall.Status)
				assert.Equal(t, updates[0].ToolCall.ToolCallId, updates[1].ToolCallUpdate.ToolCallId)
				assert.Equal(t, acpsdk.ToolCallStatusFailed, *updates[1].ToolCallUpdate.Status)
				assert.Contains(t, updates[1].ToolCallUpdate.Content[0].Content.Content.Text.Text, "side effects may have occurred")
			})
		})
	}
}

func TestToolLifecycleDoesNotExposePartialArgumentsOrOutput(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"secret":"partial`}}
		rt := &fakeRuntime{events: []runtime.Event{
			runtime.PartialToolCall(call, tools.Tool{Name: "shell"}, "root"),
			runtime.ToolCallOutput(call.ID, tools.Tool{Name: "shell"}, "unbuffered-secret", "root"),
		}}
		f := newRunAgentFixture(t, rt, &captureWriter{})
		require.NoError(t, f.runAgent(t.Context(), f.sess))
		assert.Empty(t, toolUpdates(f.sessionUpdates(t)))
		assert.NotContains(t, strings.Join(f.out.lines(), "\n"), "secret")
	})
}

func TestToolLifecycleRepeatedStartUpdatesExistingCall(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "shell", Arguments: "{}"}}
		tool := tools.Tool{Name: "shell"}
		start := runtime.ToolCall(call, tool, "root")
		response := runtime.ToolCallResponse("call", tool, tools.ResultSuccess("private result"), "transformed result", "root")
		rt := &fakeRuntime{events: []runtime.Event{start, start, response}}
		f := newRunAgentFixture(t, rt, &captureWriter{})
		require.NoError(t, f.runAgent(t.Context(), f.sess))
		updates := toolUpdates(f.sessionUpdates(t))
		require.Len(t, updates, 3)
		assert.Equal(t, updates[0].ToolCall.ToolCallId, updates[1].ToolCallUpdate.ToolCallId)
		assert.Equal(t, updates[0].ToolCall.ToolCallId, updates[2].ToolCallUpdate.ToolCallId)
		assert.Equal(t, acpsdk.ToolCallStatusInProgress, *updates[1].ToolCallUpdate.Status)
		assert.Equal(t, map[string]any{"content": "transformed result"}, updates[2].ToolCallUpdate.RawOutput)
		assert.NotContains(t, strings.Join(f.out.lines(), "\n"), "private result")
		assert.Equal(t, "call", start.(*runtime.ToolCallEvent).ToolCall.ID)
		assert.Equal(t, "call", response.(*runtime.ToolCallResponseEvent).ToolCallID)
	})
}
