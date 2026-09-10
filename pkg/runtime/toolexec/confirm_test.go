package toolexec_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/safety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// echoCommand is the action every gate test asks approval for: a shell
// command, the shape the skills toolset uses for the commands a skill body
// embeds.
var echoCommand = tools.ConfirmedRun{
	ToolName: safety.ShellToolName,
	Args:     map[string]any{"cmd": "rm -rf build"},
	Metadata: map[string]string{"skill": "builder"},
}

func newGate(sess *session.Session, em toolexec.Emitter, resume <-chan toolexec.ResumeRequest) *toolexec.Gate {
	return &toolexec.Gate{
		Sess:    sess,
		Agent:   newAgent(),
		Emitter: em,
		Resume:  resume,
	}
}

// execDone stands in for the action itself; the gate only cares whether it
// gets called.
func execDone(context.Context, tools.ConfirmedRun) (string, error) { return "done", nil }

// staticCheckers is the permission provider for tests whose rules don't change
// mid-flight.
func staticCheckers(checkers ...toolexec.NamedChecker) func(*session.Session) []toolexec.NamedChecker {
	return func(*session.Session) []toolexec.NamedChecker { return checkers }
}

// sessionCheckers mirrors the runtime's provider: it rebuilds the session-tier
// checker on every call so a grant made mid-flight takes effect.
func sessionCheckers(sess *session.Session) []toolexec.NamedChecker {
	perms := sess.ClonePermissions()
	if perms == nil {
		return nil
	}
	return []toolexec.NamedChecker{{
		Checker: permissions.NewCheckerFromRules(perms.Allow, perms.Ask, perms.Deny),
		Source:  "session permissions",
		Tier:    toolexec.TierSession,
	}}
}

func TestGate_PreToolUseDenyBlocksNestedAction(t *testing.T) {
	t.Parallel()
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	em := &captureEmitter{}
	hd := &stubHookDispatcher{on: map[hooks.EventType]*hooks.Result{
		hooks.EventPreToolUsePreYolo: {
			Allowed:        false,
			Decision:       hooks.DecisionDeny,
			DecisionReason: "blocked by policy",
		},
	}}
	g := newGate(sess, em, nil)
	g.Hooks = hd

	ran := false
	_, err := g.ConfirmAndRun(t.Context(), echoCommand, func(context.Context, tools.ConfirmedRun) (string, error) {
		ran = true
		return "", nil
	})

	require.ErrorIs(t, err, tools.ErrConfirmationDenied)
	assert.False(t, ran)
	assert.Contains(t, hd.dispatched, hooks.EventPreToolUsePreYolo)
	assert.Empty(t, em.responses, "an unprompted policy denial has no synthetic call lifecycle")
	assert.Empty(t, em.messages)
}

func TestGate_PreToolUseRewriteIsExecuted(t *testing.T) {
	t.Parallel()
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyBalanced))
	em := &captureEmitter{}
	hd := &stubHookDispatcher{on: map[hooks.EventType]*hooks.Result{
		hooks.EventPreToolUse: {
			Allowed:       true,
			Decision:      hooks.DecisionAllow,
			ModifiedInput: map[string]any{"cmd": "git status", "cwd": "/tmp"},
		},
	}}
	g := newGate(sess, em, nil)
	g.Hooks = hd

	var approved tools.ConfirmedRun
	_, err := g.ConfirmAndRun(t.Context(), echoCommand, func(_ context.Context, run tools.ConfirmedRun) (string, error) {
		approved = run
		return "ok", nil
	})

	require.NoError(t, err)
	assert.Equal(t, "git status", approved.Args["cmd"])
	assert.Equal(t, "/tmp", approved.Args["cwd"])
}

func TestGate_TransformsOutputBeforeReturning(t *testing.T) {
	t.Parallel()
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	em := &captureEmitter{}
	redacted := "[REDACTED]"
	hd := &stubHookDispatcher{on: map[hooks.EventType]*hooks.Result{
		hooks.EventToolResponseTransform: {
			Allowed:             true,
			UpdatedToolResponse: &redacted,
		},
	}}
	g := newGate(sess, em, nil)
	g.Hooks = hd

	output, err := g.ConfirmAndRun(t.Context(), echoCommand, func(context.Context, tools.ConfirmedRun) (string, error) {
		return "secret", nil
	})

	require.NoError(t, err)
	assert.Equal(t, redacted, output)
	require.Len(t, em.responses, 1)
	assert.Equal(t, redacted, em.responses[0].Output)
	assert.Contains(t, hd.dispatched, hooks.EventToolResponseTransform)
}

func TestGate_InvalidArgumentsFailClosed(t *testing.T) {
	t.Parallel()
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	em := &captureEmitter{}
	invalid := tools.ConfirmedRun{
		ToolName: safety.ShellToolName,
		Args:     map[string]any{"cmd": func() {}},
	}

	ran := false
	_, err := newGate(sess, em, nil).ConfirmAndRun(t.Context(), invalid, func(context.Context, tools.ConfirmedRun) (string, error) {
		ran = true
		return "", nil
	})

	require.Error(t, err)
	assert.False(t, ran)
	assert.Empty(t, em.calls)
}

func TestGate_UsesSharedConfirmationMutex(t *testing.T) {
	t.Parallel()
	sess := session.New()
	em := &captureEmitter{confirmed: make(chan struct{})}
	resume := make(chan toolexec.ResumeRequest, 1)
	g := newGate(sess, em, resume)
	var mu sync.Mutex
	g.Mu = &mu

	mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := g.ConfirmAndRun(t.Context(), echoCommand, execDone)
		done <- err
	}()

	select {
	case <-em.confirmed:
		t.Fatal("confirmation emitted before the shared mutex was released")
	case <-time.After(20 * time.Millisecond):
	}
	mu.Unlock()
	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeApprove}
	require.NoError(t, <-done)
}

func TestGate_AutonomousRunsWithoutPrompting(t *testing.T) {
	t.Parallel()
	sess := session.New()
	sess.SetSafetyPolicy(session.SafetyPolicyAutonomous)
	em := &captureEmitter{}

	output, err := newGate(sess, em, nil).ConfirmAndRun(t.Context(), echoCommand, execDone)

	require.NoError(t, err)
	assert.Equal(t, "done", output)
	assert.Empty(t, em.confirmations)
	require.Len(t, em.calls, 1, "an approved action must show up in the transcript")
	require.Len(t, em.responses, 1)
	assert.False(t, em.responses[0].IsError)
	assert.Empty(t, em.messages, "an out-of-band action must not be recorded in the conversation")
}

func TestGate_DeniedByPermissionRuleSkipsExec(t *testing.T) {
	t.Parallel()
	sess := session.New()
	em := &captureEmitter{}
	g := newGate(sess, em, nil)
	g.Permissions = staticCheckers(toolexec.NamedChecker{
		Checker: newDenyChecker(safety.ShellToolName),
		Source:  "permissions configuration",
		Tier:    toolexec.TierTeam,
	})

	ran := false
	_, err := g.ConfirmAndRun(t.Context(), echoCommand, func(context.Context, tools.ConfirmedRun) (string, error) {
		ran = true
		return "", nil
	})

	require.ErrorIs(t, err, tools.ErrConfirmationDenied)
	assert.False(t, ran)
	assert.Empty(t, em.confirmations)
	assert.Empty(t, em.responses, "an unprompted policy denial has no synthetic call lifecycle")
}

func TestGate_NonInteractiveSessionDenies(t *testing.T) {
	t.Parallel()
	sess := session.New()
	sess.NonInteractive = true
	em := &captureEmitter{}

	_, err := newGate(sess, em, nil).ConfirmAndRun(t.Context(), echoCommand, execDone)

	require.ErrorIs(t, err, tools.ErrConfirmationDenied)
	assert.Empty(t, em.confirmations, "nobody is there to answer a prompt")
}

func TestGate_UserApprovalRunsTheAction(t *testing.T) {
	t.Parallel()
	sess := session.New()
	em := &captureEmitter{confirmed: make(chan struct{})}
	resume := make(chan toolexec.ResumeRequest, 1)
	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeApprove}

	output, err := newGate(sess, em, resume).ConfirmAndRun(t.Context(), echoCommand, execDone)

	require.NoError(t, err)
	assert.Equal(t, "done", output)
	require.Len(t, em.confirmations, 1)
	assert.Equal(t, safety.ShellToolName, em.confirmations[0].Function.Name)
	assert.JSONEq(t, `{"cmd":"rm -rf build"}`, em.confirmations[0].Function.Arguments)
	require.Len(t, em.confirmationMeta, 1)
	assert.Equal(t, "builder", em.confirmationMeta[0]["skill"], "the prompt must name the skill the command comes from")
	assert.Equal(t, string(safety.ClassDestructive), em.confirmationMeta[0][safety.MetaSafetyLabel])
}

func TestGate_UserRejectionSkipsExec(t *testing.T) {
	t.Parallel()
	sess := session.New()
	em := &captureEmitter{confirmed: make(chan struct{})}
	resume := make(chan toolexec.ResumeRequest, 1)
	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeReject, Reason: "not now"}

	ran := false
	_, err := newGate(sess, em, resume).ConfirmAndRun(t.Context(), echoCommand, func(context.Context, tools.ConfirmedRun) (string, error) {
		ran = true
		return "", nil
	})

	require.ErrorIs(t, err, tools.ErrConfirmationDenied)
	require.ErrorContains(t, err, "not now")
	assert.False(t, ran)
	require.Len(t, em.responses, 1)
	assert.True(t, em.responses[0].IsError)
	assert.Len(t, em.calls, 1, "the refused action still shows up in the transcript")
}

func TestGate_ApproveToolGrantsSessionPermission(t *testing.T) {
	t.Parallel()
	sess := session.New()
	em := &captureEmitter{confirmed: make(chan struct{})}
	resume := make(chan toolexec.ResumeRequest, 1)
	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeApproveTool}

	_, err := newGate(sess, em, resume).ConfirmAndRun(t.Context(), echoCommand, execDone)

	require.NoError(t, err)
	perms := sess.ClonePermissions()
	require.NotNil(t, perms)
	assert.Contains(t, perms.Allow, safety.ShellToolName)
}

// TestGate_RechecksPolicyAfterWaitingForTheLock pins that a grant made while a
// gate was queued behind another prompt takes effect instead of being asked
// about again.
func TestGate_RechecksPolicyAfterWaitingForTheLock(t *testing.T) {
	t.Parallel()
	sess := session.New()
	em := &captureEmitter{}
	var mu sync.Mutex
	g := newGate(sess, em, nil)
	g.Mu = &mu

	mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := g.ConfirmAndRun(t.Context(), echoCommand, execDone)
		done <- err
	}()

	// The queued gate must observe the escalation that lands while it waits.
	sess.SetSafetyPolicy(session.SafetyPolicyAutonomous)
	mu.Unlock()

	require.NoError(t, <-done)
	assert.Empty(t, em.confirmations)
}

// TestGate_RechecksPermissionsAfterWaitingForTheLock pins that an "always
// allow this tool" grant made while a gate was queued behind another prompt is
// honoured instead of prompting again.
func TestGate_RechecksPermissionsAfterWaitingForTheLock(t *testing.T) {
	t.Parallel()
	sess := session.New()
	em := &captureEmitter{}
	var mu sync.Mutex
	g := newGate(sess, em, nil)
	g.Permissions = sessionCheckers
	g.Mu = &mu

	mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := g.ConfirmAndRun(t.Context(), echoCommand, execDone)
		done <- err
	}()

	sess.AppendPermissionAllow(safety.ShellToolName)
	mu.Unlock()

	require.NoError(t, <-done)
	assert.Empty(t, em.confirmations)
}

func TestGate_ExecutionErrorIsTransformed(t *testing.T) {
	t.Parallel()
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	em := &captureEmitter{}
	redacted := "redacted error"
	cause := errors.New("secret error")
	hd := &stubHookDispatcher{on: map[hooks.EventType]*hooks.Result{
		hooks.EventToolResponseTransform: {
			Allowed:             true,
			UpdatedToolResponse: &redacted,
		},
	}}
	g := newGate(sess, em, nil)
	g.Hooks = hd

	_, err := g.ConfirmAndRun(t.Context(), echoCommand, func(context.Context, tools.ConfirmedRun) (string, error) {
		return "", cause
	})

	require.EqualError(t, err, redacted)
	require.ErrorIs(t, err, cause)
	require.Len(t, em.responses, 1)
	assert.Equal(t, redacted, em.responses[0].Output)
	assert.True(t, em.responses[0].IsError)
	assert.Contains(t, hd.dispatched, hooks.EventPostToolUse)
}

func TestGate_PreservesExecutionCancellation(t *testing.T) {
	t.Parallel()
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	em := &captureEmitter{}

	_, err := newGate(sess, em, nil).ConfirmAndRun(t.Context(), echoCommand, func(context.Context, tools.ConfirmedRun) (string, error) {
		return "", context.Canceled
	})

	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, em.responses, 1)
	assert.True(t, em.responses[0].IsError)
}

func TestGate_ShellActionCarriesShellCategory(t *testing.T) {
	t.Parallel()
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	em := &captureEmitter{}
	hd := &stubHookDispatcher{}
	g := newGate(sess, em, nil)
	g.Hooks = hd

	_, err := g.ConfirmAndRun(t.Context(), echoCommand, execDone)

	require.NoError(t, err)
	require.NotNil(t, hd.lastTransformInput)
	assert.Equal(t, "shell", hd.lastTransformInput.ToolCategory)
}

func TestGate_PostToolUseStopAbortsExpansion(t *testing.T) {
	t.Parallel()
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	em := &captureEmitter{}
	hd := &stubHookDispatcher{on: map[hooks.EventType]*hooks.Result{
		hooks.EventPostToolUse: {
			Allowed: false,
			Message: "stop requested",
		},
	}}
	g := newGate(sess, em, nil)
	g.Hooks = hd

	_, err := g.ConfirmAndRun(t.Context(), echoCommand, execDone)

	var stop *toolexec.StopRunError
	require.ErrorAs(t, err, &stop)
	assert.Equal(t, "stop requested", stop.Message)
	var abort interface{ AbortExpansion() }
	assert.ErrorAs(t, err, &abort)
}

func TestGate_ExecFailurePropagates(t *testing.T) {
	t.Parallel()
	sess := session.New()
	sess.SetSafetyPolicy(session.SafetyPolicyAutonomous)
	em := &captureEmitter{}

	_, err := newGate(sess, em, nil).ConfirmAndRun(t.Context(), echoCommand, func(context.Context, tools.ConfirmedRun) (string, error) {
		return "", assert.AnError
	})

	require.EqualError(t, err, assert.AnError.Error())
	require.Len(t, em.responses, 1)
	assert.True(t, em.responses[0].IsError)
}

func TestGate_SafeCommandIsAllowedUnderBalanced(t *testing.T) {
	t.Parallel()
	sess := session.New()
	sess.SetSafetyPolicy(session.SafetyPolicyBalanced)
	em := &captureEmitter{}

	safeCommand := tools.ConfirmedRun{
		ToolName: safety.ShellToolName,
		Args:     map[string]any{"cmd": "git status"},
	}
	_, err := newGate(sess, em, nil).ConfirmAndRun(t.Context(), safeCommand, execDone)

	require.NoError(t, err)
	assert.Empty(t, em.confirmations, "balanced mode does not prompt for a read-only command")
}

// TestGate_HonoursSessionAllowRule pins that the gate goes through the same
// permission layers as a real tool call, so a session-scoped grant covers the
// commands a skill embeds too.
func TestGate_HonoursSessionAllowRule(t *testing.T) {
	t.Parallel()
	sess := session.New()
	em := &captureEmitter{}
	g := newGate(sess, em, nil)
	g.Permissions = staticCheckers(toolexec.NamedChecker{
		Checker: permissions.NewCheckerFromRules([]string{safety.ShellToolName}, nil, nil),
		Source:  "session permissions",
		Tier:    toolexec.TierSession,
	})

	_, err := g.ConfirmAndRun(t.Context(), echoCommand, execDone)

	require.NoError(t, err)
	assert.Empty(t, em.confirmations)
}
