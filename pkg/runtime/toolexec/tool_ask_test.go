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

// With nobody at the keyboard a mandatory Ask must deny, even under
// autonomous mode and with a matching session grant.
func TestToolPhases_MandatoryAskDeniesNonInteractive(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		cfg  *hooks.Config
	}{
		{name: "tool_guard ask", cfg: &hooks.Config{ToolGuard: matchAll(hook(bVerdict, "ask", "needs a human"))}},
		{name: "preempt ask", cfg: &hooks.Config{PreToolUse: preemptYolo(hook(bVerdict, "ask", "needs a human"))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.cfg.PermissionRequest = matchAll(hook(bPermit))
			hd, rec := newPhaseHooks(t, tc.cfg)
			sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
			sess.NonInteractive = true
			sess.Permissions = &session.PermissionsConfig{Allow: []string{"shell:cmd=rm*"}}
			d := newDispatcher(hd, nil)
			d.Permissions = sessionCheckers
			em := &captureEmitter{}

			d.Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "rm -rf / && echo")}, []tools.Tool{neverRun()}, em)

			requireDenied(t, em, hd, toolexec.ApprovalSourceNonInteractiveDeny, "requires user confirmation but the session is non-interactive")
			assert.Empty(t, em.hookBlocks, "a non-interactive deny is not a hook block")
			assert.Zero(t, rec.count(hooks.EventPermissionRequest), "permission_request must not fire behind a mandatory ask")
		})
	}
}

// When both mandatory lanes ask, the user is prompted once with both lanes'
// metadata (guard wins a key clash), permission_request stays skipped and the
// session grant does not silence the prompt.
func TestToolPhases_GuardAndPreemptAskPromptOnce(t *testing.T) {
	t.Parallel()
	hd, rec := newPhaseHooks(t, &hooks.Config{
		ToolGuard: matchAll(hook(bVerdict, "ask", "guard says review", "category=deploy", "owner=guard")),
		PreToolUse: append(
			preemptYolo(hook(bVerdict, "ask", "preempt says review", "category=fs-delete", "blast_radius=high")),
			matchAll(hook(bVerdict, "allow", "legacy would allow"))...,
		),
		PermissionRequest: matchAll(hook(bPermit)),
	})
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	sess.Permissions = &session.PermissionsConfig{Allow: []string{"shell:cmd=mkdir*"}}
	resume := make(chan toolexec.ResumeRequest, 1)
	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeApprove}
	d := newDispatcher(hd, resume)
	d.Permissions = sessionCheckers
	var got []string
	em := &captureEmitter{}

	d.Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "mkdir out")}, []tools.Tool{shellTool(&got)}, em)

	require.Len(t, em.confirmations, 1)
	assert.Equal(t, []string{`{"cmd":"mkdir out"}`}, got)
	meta := em.confirmationMeta[0]
	assert.Equal(t, "deploy", meta["category"], "guard metadata outranks the preempt lane")
	assert.Equal(t, "guard", meta["owner"])
	assert.Equal(t, "high", meta["blast_radius"], "preempt metadata still reaches the prompt")
	assert.Equal(t, 1, rec.count(hooks.EventToolGuard))
	assert.Equal(t, 1, hd.count(hooks.EventPreToolUsePreYolo))
	assert.Zero(t, rec.count(hooks.EventPermissionRequest))
	assert.Zero(t, hd.count(hooks.EventPreToolUse), "default lane is skipped once a mandatory lane asks")
	assert.Equal(t, approvalRecord{Decision: toolexec.ApprovalDecisionAllow, Source: toolexec.ApprovalSourceUserApproved}, lastApproval(t, hd))
}
