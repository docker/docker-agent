package toolexec_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// rewriteToRmHooks wires the two legacy pre_tool_use lanes so the preempt
// lane has no opinion on the original command and only asks once the
// default lane has rewritten it to a destructive one.
func rewriteToRmHooks() *hooks.Config {
	return &hooks.Config{
		PreToolUse: append(
			preemptYolo(hook(bVerdictWhen, "rm", "ask", "destructive")),
			matchAll(hook(bRewrite, "cmd", "rm -rf /"))...,
		),
	}
}

// After a legacy rewrite the recheck must validate the rewritten arguments
// against permission rules before the preempt lane's Ask path can prompt or
// run the tool. A rewrite may only tighten the verdict: a deny rule that
// matches the rewritten command wins over the preempt Ask and over the
// session-grant exception that Ask carries.
func TestToolPhases_LegacyRewriteRecheckValidatesRulesBeforePreemptAsk(t *testing.T) {
	t.Parallel()

	t.Run("preempt ask must not prompt for a denied rewrite", func(t *testing.T) {
		t.Parallel()
		hd, _ := newPhaseHooks(t, rewriteToRmHooks())
		sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyStrict))
		resume := make(chan toolexec.ResumeRequest, 1)
		resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeApprove}
		d := newDispatcher(hd, resume)
		d.Permissions = teamRules(nil, []string{"shell:cmd=rm*"})
		var got []string
		em := &captureEmitter{}

		d.Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "ls")}, []tools.Tool{shellTool(&got)}, em)

		assert.Empty(t, got, "rewritten command matches a deny rule and must never run")
		requireDenied(t, em, hd, toolexec.ApprovalSourceTeamPermissionsDeny, "denied by permissions configuration")
		assert.Equal(t, 2, hd.count(hooks.EventPreToolUsePreYolo), "preempt lane still rechecked on the rewrite")
	})

	t.Run("session grant must not run a denied rewrite", func(t *testing.T) {
		t.Parallel()
		hd, _ := newPhaseHooks(t, rewriteToRmHooks())
		sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyStrict))
		// The grant is only visible to the preempt Ask shortcut: the
		// provider carries the team deny rule alone, so the rules recheck
		// must deny the rewrite regardless of what the session allows.
		sess.Permissions = &session.PermissionsConfig{Allow: []string{"shell:cmd=rm*"}}
		d := newDispatcher(hd, nil)
		d.Permissions = teamRules(nil, []string{"shell:cmd=rm*"})
		var got []string
		em := &captureEmitter{}

		d.Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "ls")}, []tools.Tool{shellTool(&got)}, em)

		assert.Empty(t, got, "session grant must not run the tool before the rewritten rules are checked")
		requireDenied(t, em, hd, toolexec.ApprovalSourceTeamPermissionsDeny, "denied by permissions configuration")
	})

	// Control: without a deny rule the preempt Ask on the rewrite still
	// prompts and the user's answer is honored, so the fix must not turn
	// the recheck into a blanket deny.
	t.Run("preempt ask on the rewrite still prompts when rules allow", func(t *testing.T) {
		t.Parallel()
		hd, _ := newPhaseHooks(t, rewriteToRmHooks())
		sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyStrict))
		resume := make(chan toolexec.ResumeRequest, 1)
		resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeReject, Reason: "no thanks"}
		var got []string
		em := &captureEmitter{}

		newDispatcher(hd, resume).Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "ls")}, []tools.Tool{shellTool(&got)}, em)

		assert.Empty(t, got)
		require.Len(t, em.confirmations, 1)
		assert.JSONEq(t, `{"cmd":"rm -rf /"}`, em.confirmations[0].Function.Arguments)
		require.Len(t, em.responses, 1)
		assert.Contains(t, em.responses[0].Output, "no thanks")
	})
}

func TestToolPhases_RewrittenAskCannotUseEarlierGrants(t *testing.T) {
	t.Parallel()
	for _, legacyAsk := range []bool{false, true} {
		t.Run(map[bool]string{false: "preempt ask", true: "approval hook ask"}[legacyAsk], func(t *testing.T) {
			t.Parallel()
			cfg := rewriteToRmHooks()
			if legacyAsk {
				cfg.PreToolUse = matchAll(hook(bRewrite, "cmd", "rm -rf /"), hook(bVerdict, "ask", "review rewritten command"))
			}
			cfg.PermissionRequest = matchAll(hook(bPermit))
			hd, _ := newPhaseHooks(t, cfg)
			sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyStrict))
			sess.Permissions = &session.PermissionsConfig{Allow: []string{"shell:cmd=rm*"}}
			resume := make(chan toolexec.ResumeRequest, 1)
			resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeReject}
			d := newDispatcher(hd, resume)
			d.Permissions = sessionCheckers
			em := &captureEmitter{}
			d.Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "ls")}, []tools.Tool{neverRun()}, em)
			require.Len(t, em.confirmations, 1)
			assert.Zero(t, hd.count(hooks.EventPermissionRequest))
			assert.Equal(t, toolexec.ApprovalSourceUserRejected, lastApproval(t, hd).Source)
		})
	}
}
