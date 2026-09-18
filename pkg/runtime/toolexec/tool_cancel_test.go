package toolexec_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// bWait blocks until its context dies and reports the context error, so the
// dispatcher sees exactly what the executor's fail-closed path produces.
const bWait = "wait"

// newWaitHooks builds a real executor whose hooks block. started is closed
// once the first hook is running so tests can cancel mid-dispatch.
func newWaitHooks(t *testing.T, cfg *hooks.Config) (*execHookDispatcher, <-chan struct{}) {
	t.Helper()
	started := make(chan struct{})
	var once sync.Once
	reg := hooks.NewRegistry()
	require.NoError(t, reg.RegisterBuiltin(bWait, func(ctx context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	return &execHookDispatcher{exec: hooks.NewExecutorWithRegistry(cfg, t.TempDir(), nil, reg)}, started
}

func cancelWhenStarted(t *testing.T, started <-chan struct{}) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() {
		select {
		case <-started:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx
}

func requireCanceled(t *testing.T, em *captureEmitter, hd *execHookDispatcher) {
	t.Helper()
	assert.Empty(t, em.confirmations)
	assert.Empty(t, em.hookBlocks, "a cancellation is not a hook denial")
	require.Len(t, em.responses, 1)
	assert.True(t, em.responses[0].IsError)
	assert.Contains(t, em.responses[0].Output, "canceled by the user")
	assert.Equal(t, approvalRecord{Decision: toolexec.ApprovalDecisionCanceled, Source: toolexec.ApprovalSourceContextCanceled}, lastApproval(t, hd))
}

// The executor fails closed when its context dies, so a user cancel that
// lands while a pre-execution hook runs must still be reported as a
// cancellation, never as a hook denial — and the tool must not run.
func TestToolPhases_ParentCancelDuringHookIsCanceled(t *testing.T) {
	t.Parallel()

	blocking := hook(bWait)
	blocking.OnError = "block" // events that do not fail closed on their own

	for _, tc := range []struct {
		name   string
		cfg    *hooks.Config
		policy session.SafetyPolicy
	}{
		{name: "tool_input_transform", cfg: &hooks.Config{ToolInputTransform: matchAll(blocking)}, policy: session.SafetyPolicyAutonomous},
		{name: "tool_guard", cfg: &hooks.Config{ToolGuard: matchAll(hook(bWait))}, policy: session.SafetyPolicyAutonomous},
		{name: "pre_tool_use preempt lane", cfg: &hooks.Config{PreToolUse: preemptYolo(hook(bWait))}, policy: session.SafetyPolicyAutonomous},
		{name: "pre_tool_use default lane", cfg: &hooks.Config{PreToolUse: matchAll(hook(bWait))}, policy: session.SafetyPolicyStrict},
		{name: "permission_request", cfg: &hooks.Config{PermissionRequest: matchAll(blocking)}, policy: session.SafetyPolicyStrict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hd, started := newWaitHooks(t, tc.cfg)
			sess := session.New(session.WithSafetyPolicy(tc.policy))
			var got []string
			em := &captureEmitter{}

			stop, msg := newDispatcher(hd, nil).Process(cancelWhenStarted(t, started), sess, []tools.ToolCall{shellCall("x", "ls")}, []tools.Tool{shellTool(&got)}, em)

			assert.False(t, stop)
			assert.Empty(t, msg)
			assert.Empty(t, got, "tool must not run after a cancel")
			requireCanceled(t, em, hd)
		})
	}
}

// A cancel during one call's guard cancels its batch siblings too, exactly
// like a cancel during a confirmation prompt.
func TestToolPhases_ParentCancelDuringGuardCancelsBatch(t *testing.T) {
	t.Parallel()
	hd, started := newWaitHooks(t, &hooks.Config{ToolGuard: matchAll(hook(bWait))})
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	var got []string
	em := &captureEmitter{}

	newDispatcher(hd, nil).Process(cancelWhenStarted(t, started), sess, []tools.ToolCall{
		shellCall("a", "ls"),
		shellCall("b", "pwd"),
	}, []tools.Tool{shellTool(&got)}, em)

	assert.Empty(t, got)
	assert.Empty(t, em.hookBlocks)
	require.Len(t, em.responses, 2)
	for _, r := range em.responses {
		assert.True(t, r.IsError)
		assert.Contains(t, r.Output, "canceled")
	}
	hd.mu.Lock()
	defer hd.mu.Unlock()
	require.Len(t, hd.approvals, 2)
	for _, ap := range hd.approvals {
		assert.Equal(t, approvalRecord{Decision: toolexec.ApprovalDecisionCanceled, Source: toolexec.ApprovalSourceContextCanceled}, ap)
	}
}

func TestGate_ParentCancelDuringGuardIsCanceled(t *testing.T) {
	t.Parallel()
	hd, started := newWaitHooks(t, &hooks.Config{ToolGuard: matchAll(hook(bWait))})
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	em := &captureEmitter{}
	g := newGate(sess, em, nil)
	g.Hooks = hd

	ran := false
	_, err := g.ConfirmAndRun(cancelWhenStarted(t, started), echoCommand, func(context.Context, tools.ConfirmedRun) (string, error) {
		ran = true
		return "", nil
	})

	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, tools.ErrConfirmationDenied)
	assert.False(t, ran)
	assert.Empty(t, em.hookBlocks)
	assert.Empty(t, em.responses, "an unprompted cancellation has no synthetic call lifecycle")
	assert.Equal(t, approvalRecord{Decision: toolexec.ApprovalDecisionCanceled, Source: toolexec.ApprovalSourceContextCanceled}, lastApproval(t, hd))
}

// A hook's own timeout only cancels the hook's context: with the parent
// alive it is a real failure and the mandatory lanes keep failing closed.
func TestToolPhases_HookTimeoutStaysDenied(t *testing.T) {
	t.Parallel()

	slow := hook(bWait)
	slow.Timeout = 1

	for _, tc := range []struct {
		name   string
		cfg    *hooks.Config
		source string
		want   []string
	}{
		{
			name:   "tool_guard",
			cfg:    &hooks.Config{ToolGuard: matchAll(slow)},
			source: toolexec.ApprovalSourceToolGuardDeny,
			want:   []string{"tool_guard hook failed to execute", "timed out after 1s"},
		},
		{
			name:   "pre_tool_use preempt lane",
			cfg:    &hooks.Config{PreToolUse: preemptYolo(slow)},
			source: toolexec.ApprovalSourcePreToolUseHookDeny,
			want:   []string{"pre_tool_use hook failed to execute", "timed out after 1s"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hd, _ := newWaitHooks(t, tc.cfg)
			sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
			em := &captureEmitter{}

			newDispatcher(hd, nil).Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "ls")}, []tools.Tool{neverRun()}, em)

			requireDenied(t, em, hd, tc.source, tc.want...)
			require.Len(t, em.hookBlocks, 1)
		})
	}
}
