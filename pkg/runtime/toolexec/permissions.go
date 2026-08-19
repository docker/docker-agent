package toolexec

import (
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/safety"
	"github.com/docker/docker-agent/pkg/session"
)

// PermissionOutcome is the resolved decision after evaluating the full
// approval pipeline.
type PermissionOutcome int

const (
	// OutcomeAllow means the tool can run without asking the user.
	OutcomeAllow PermissionOutcome = iota
	// OutcomeDeny means the tool must be rejected; the caller should
	// surface a tool-error response that mentions Source.
	OutcomeDeny
	// OutcomeAsk means the user must be asked for explicit confirmation.
	OutcomeAsk
)

// PermissionReason explains *why* a [PermissionDecision] was reached.
// Callers use it to produce accurate log messages and to know which
// stage produced the verdict (custom rule vs. safety mode).
type PermissionReason int

const (
	// ReasonChecker: a configured permission checker (session-level or
	// team-level custom rules) produced a definitive Allow/Deny/Ask
	// verdict. PermissionDecision.Source identifies which checker.
	ReasonChecker PermissionReason = iota
	// ReasonMode: no custom rule matched; the session's safety mode
	// was applied against the call's safety label.
	ReasonMode
)

// Tier identifies which layer a permission checker belongs to. Session
// rules are direct user intent (interactive grants, the session
// permissions API); team rules come from the agent YAML and user-global
// config. The tiers differ in one way: a session `ask:` rule always
// prompts, while a team `ask:` rule yields to a user-chosen
// auto-deciding mode (Balanced / Restricted / Autonomous).
type Tier int

const (
	TierSession Tier = iota
	TierTeam
)

// NamedChecker pairs a [permissions.Checker] with its [Tier] and a
// human-readable source label (e.g. "session permissions",
// "permissions configuration") used in denial messages and debug logs.
type NamedChecker struct {
	Checker *permissions.Checker
	Source  string
	Tier    Tier
}

// PermissionDecision is the result of [Decide]: an outcome plus the
// reason and the source that produced it — a checker label when the
// reason is [ReasonChecker], an ApprovalSource* constant when the
// reason is [ReasonMode].
type PermissionDecision struct {
	Outcome PermissionOutcome
	Reason  PermissionReason
	Source  string
}

// Decide resolves the permission outcome for a tool call:
//
//  1. Custom rules, walked in checker order (session tier first, then
//     team tier). Deny and Allow win outright — a deny rule blocks
//     even Autonomous, an allow rule silences even Strict and
//     overrides Restricted's fail-closed fallback. An explicit ask
//     rule prompts, except that a team-tier ask yields to
//     Balanced/Restricted/Autonomous (the user's chosen mode outranks
//     the agent author's advisory; under Restricted this keeps the
//     profile prompt-free instead of introducing implicit asks).
//  2. No rule matched: the (mode × label) table via [applyMode].
//
// Decide is pure (no I/O, no side effects) so the entire approval
// matrix can be exhaustively unit-tested without a runtime.
func Decide(
	mode session.SafetyPolicy,
	label safety.Label,
	checkers []NamedChecker,
	toolName string,
	toolArgs map[string]any,
) PermissionDecision {
	autoDeciding := mode == session.SafetyPolicyBalanced ||
		mode == session.SafetyPolicyRestricted ||
		mode == session.SafetyPolicyAutonomous
	for _, pc := range checkers {
		switch pc.Checker.CheckWithArgs(toolName, toolArgs) {
		case permissions.Deny:
			return PermissionDecision{Outcome: OutcomeDeny, Reason: ReasonChecker, Source: pc.Source}
		case permissions.Allow:
			return PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonChecker, Source: pc.Source}
		case permissions.ForceAsk:
			if pc.Tier == TierSession || !autoDeciding {
				return PermissionDecision{Outcome: OutcomeAsk, Reason: ReasonChecker, Source: pc.Source}
			}
			// Team-tier ask is advisory under Balanced/Restricted/
			// Autonomous; fall through to the mode.
		case permissions.Ask:
			// No explicit match at this level; fall through to next checker.
		}
	}
	return applyMode(mode, label)
}

// applyMode implements the (mode × label) → verdict table:
//
//	              safe     destructive   unknown
//	strict        ask      ask           ask
//	balanced      ALLOW    ask           ask
//	restricted    ALLOW    DENY          DENY
//	autonomous    ALLOW    ALLOW         ALLOW
//	"" (legacy)   ask*     ask           ask
//
// (*) the legacy default — a session whose mode was never explicitly
// chosen — additionally auto-approves annotation-safe (read-only
// hinted) calls, but only AFTER the default pre_tool_use hook chain
// has had its turn; see the dispatcher's approveAndRun. That keeps the
// pre-modes contract: hooks can veto read-only calls, and read-only
// tools never prompt. Restricted is the fail-closed profile for
// unattended runs: it never prompts, denying whatever it would not
// allow. Unrecognised modes are treated as strict so an invalid value
// can never widen approval.
func applyMode(mode session.SafetyPolicy, label safety.Label) PermissionDecision {
	switch mode {
	case session.SafetyPolicyAutonomous:
		return PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonMode, Source: ApprovalSourceYolo}
	case session.SafetyPolicyBalanced:
		if label.Class == safety.ClassSafe {
			return PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonMode, Source: ApprovalSourceModeBalanced}
		}
		return PermissionDecision{Outcome: OutcomeAsk, Reason: ReasonMode, Source: ApprovalSourceModeBalanced}
	case session.SafetyPolicyRestricted:
		if label.Class == safety.ClassSafe {
			return PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonMode, Source: ApprovalSourceModeRestricted}
		}
		return PermissionDecision{Outcome: OutcomeDeny, Reason: ReasonMode, Source: ApprovalSourceModeRestricted}
	case session.SafetyPolicyStrict:
		return PermissionDecision{Outcome: OutcomeAsk, Reason: ReasonMode, Source: ApprovalSourceModeStrict}
	default:
		return PermissionDecision{Outcome: OutcomeAsk, Reason: ReasonMode, Source: ApprovalSourceModeLegacy}
	}
}

// legacyReadOnlyAutoApprove reports whether the legacy default mode
// auto-approves this call: no mode was ever chosen and the tool
// advertises itself read-only via annotations. Classifier-derived
// safety deliberately does not qualify — the legacy default predates
// the classifier and must not widen.
func legacyReadOnlyAutoApprove(mode session.SafetyPolicy, label safety.Label) bool {
	return mode == "" && label.Class == safety.ClassSafe && label.Origin == safety.OriginAnnotation
}
