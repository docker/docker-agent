package toolexec

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/docker/docker-agent/pkg/hooks"
)

// transformToolInput runs once, before any guard, classifier, or permission rule.
func (c *call) transformToolInput(ctx context.Context) (CallOutcome, bool) {
	if c.d.Hooks == nil {
		return CallOutcome{}, false
	}
	in := NewHooksInput(c.sess, c.tc)
	in.ToolCategory = c.tool.Category
	result := c.d.Hooks.Dispatch(ctx, c.a, hooks.EventToolInputTransform, in)
	if outcome, canceled := c.hookCanceled(ctx); canceled {
		return outcome, true
	}
	if result == nil {
		return CallOutcome{}, false
	}
	if !result.Allowed {
		c.blockToolHook(ctx, hooks.EventToolInputTransform, ApprovalSourceToolInputTransformDeny, result.Message)
		return CallOutcome{}, true
	}
	if _, err := c.applyHookModifiedInput(result); err != nil {
		c.blockToolHook(ctx, hooks.EventToolInputTransform, ApprovalSourceToolInputTransformDeny, fmt.Sprintf("invalid rewritten input: %v", err))
		return CallOutcome{}, true
	}
	return CallOutcome{}, false
}

func (c *call) consultToolGuard(ctx context.Context) *hooks.Result {
	if !c.guardComputed {
		c.guardComputed = true
		if c.d.Hooks != nil {
			in := NewHooksInput(c.sess, c.tc)
			in.ToolCategory = c.tool.Category
			c.guardResult = c.d.Hooks.Dispatch(ctx, c.a, hooks.EventToolGuard, in)
		}
	}
	return c.guardResult
}

func (c *call) toolGuardAsks() bool {
	return c.guardResult != nil && c.guardResult.Decision == hooks.DecisionAsk
}

// consultMandatoryHooks evaluates both guard lanes without approving or prompting.
func (c *call) consultMandatoryHooks(ctx context.Context) (CallOutcome, bool) {
	guard := c.consultToolGuard(ctx)
	if outcome, canceled := c.hookCanceled(ctx); canceled {
		return outcome, true
	}
	if guard != nil && (!guard.Allowed || guard.Decision == hooks.DecisionDeny) {
		c.blockToolHook(ctx, hooks.EventToolGuard, ApprovalSourceToolGuardDeny, cmp.Or(guard.Message, guard.DecisionReason))
		return CallOutcome{}, true
	}
	preempt := c.consultPreToolUsePreYolo(ctx)
	if outcome, canceled := c.hookCanceled(ctx); canceled {
		return outcome, true
	}
	if preempt != nil && (!preempt.Allowed || preempt.Decision == hooks.DecisionDeny) {
		c.blockToolHook(ctx, hooks.EventPreToolUse, ApprovalSourcePreToolUseHookDeny, cmp.Or(preempt.Message, preempt.DecisionReason))
		return CallOutcome{}, true
	}
	return CallOutcome{}, false
}

func (c *call) runToolGuards(ctx context.Context, runTool func() CallOutcome) (CallOutcome, bool) {
	if outcome, handled := c.consultMandatoryHooks(ctx); handled {
		return outcome, true
	}
	preempt := c.preYoloResult

	if c.toolGuardAsks() {
		// A mandatory ask cannot turn an explicit policy denial into approval.
		decision := c.permissionDecision()
		if decision.Outcome == OutcomeDeny {
			c.notifyApproval(ctx, ApprovalDecisionDeny, denySourceForDecision(decision))
			c.errorResponse(ctx, denyErrorMessage(decision, c.tc.Function.Name))
			return CallOutcome{}, true
		}
		return c.askUser(ctx, runTool), true
	}
	if preempt != nil && preempt.Decision == hooks.DecisionAsk {
		// Preserve the legacy exception for informed session-scoped grants.
		if c.sessionPermissionsAllow() {
			c.notifyApproval(ctx, ApprovalDecisionAllow, ApprovalSourceSessionPermissionsAllow)
			return runTool(), true
		}
		return c.askUser(ctx, runTool), true
	}
	return CallOutcome{}, false
}

// Revalidation may deny or require fresh approval, but never auto-approve.
func (c *call) recheckRewrittenInput(ctx context.Context, runTool func() CallOutcome) (CallOutcome, bool) {
	c.guardComputed, c.preYoloComputed = false, false
	if outcome, handled := c.consultMandatoryHooks(ctx); handled {
		return outcome, true
	}
	decision := c.permissionDecision()
	if decision.Outcome == OutcomeDeny {
		c.notifyApproval(ctx, ApprovalDecisionDeny, denySourceForDecision(decision))
		c.errorResponse(ctx, denyErrorMessage(decision, c.tc.Function.Name))
		return CallOutcome{}, true
	}
	c.rewrittenInputAsk = c.rewrittenInputAsk || c.toolGuardAsks() ||
		(c.preYoloResult != nil && c.preYoloResult.Decision == hooks.DecisionAsk) ||
		(decision.Outcome == OutcomeAsk && decision.Reason == ReasonChecker)
	if c.rewrittenInputAsk {
		return c.askUser(ctx, runTool), true
	}
	return CallOutcome{}, false
}

func (c *call) mandatoryAsk() bool {
	return c.toolGuardAsks() || c.rewrittenInputAsk
}

// hookCanceled turns a parent cancellation observed after a hook dispatch
// into the canceled outcome. Hook executors fail closed when their context
// dies, so without this check a user cancel would be recorded as a hook
// denial. A hook's own timeout only cancels the child context and still
// reads as a failure.
func (c *call) hookCanceled(ctx context.Context) (CallOutcome, bool) {
	if ctx.Err() == nil {
		return CallOutcome{}, false
	}
	slog.DebugContext(ctx, "Context cancelled during hook dispatch", "tool", c.tc.Function.Name, "session_id", c.sess.ID)
	return c.canceled(ctx), true
}

// canceled records the cancellation verdict and returns the matching outcome.
func (c *call) canceled(ctx context.Context) CallOutcome {
	c.notifyApproval(ctx, ApprovalDecisionCanceled, ApprovalSourceContextCanceled)
	c.errorResponse(ctx, c.cancellationMessage(ctx))
	return c.cancellationOutcome(ctx)
}

func (c *call) blockToolHook(ctx context.Context, event hooks.EventType, source, reason string) {
	message := fmt.Sprintf("The tool call was rejected by a %s hook.", event)
	if reason = strings.TrimSpace(reason); reason != "" {
		message += " Reason: " + reason
	}
	slog.DebugContext(ctx, "Tool blocked by hook", "tool", c.tc.Function.Name, "event", event, "reason", reason, "session_id", c.sess.ID)
	c.notifyApproval(ctx, ApprovalDecisionDeny, source)
	c.em.EmitHookBlocked(c.tc, c.tool, message, c.a.Name())
	c.errorResponse(ctx, message)
}
