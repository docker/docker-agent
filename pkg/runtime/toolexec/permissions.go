package toolexec

import (
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
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
// stage of the pipeline produced the verdict (custom rule vs. safety
// mode).
type PermissionReason int

const (
	// ReasonChecker: a configured permission checker (session-level or
	// team-level) produced a definitive Allow/Deny/ForceAsk verdict.
	// PermissionDecision.Source identifies which checker.
	ReasonChecker PermissionReason = iota
	// ReasonMode: no custom checker matched; the session's SafetyPolicy
	// was applied against the tool's classifier label. Source is the
	// mode name ("strict", "balanced", "autonomous").
	ReasonMode
)

// Safety classifier labels. Every tool call carries one of these
// three; they are the runtime's canonical safety taxonomy and drive
// the mode × label verdict table.
const (
	SafetyLabelSafe        = "safe"
	SafetyLabelDestructive = "destructive"
	SafetyLabelUnknown     = "unknown"
)

// NamedChecker pairs a [permissions.Checker] with a human-readable source
// label (e.g. "session permissions", "permissions configuration") used to
// construct denial messages and debug logs.
type NamedChecker struct {
	Checker *permissions.Checker
	Source  string
}

// PermissionDecision is the result of [Decide]: an outcome plus the
// reason and (when the reason is [ReasonChecker]) the source label of the
// checker that produced it. When the reason is [ReasonMode], Source
// carries the mode name.
type PermissionDecision struct {
	Outcome PermissionOutcome
	Reason  PermissionReason
	Source  string
}

// SessionPermissionsSource labels the session-scoped checker layer.
// Session ForceAsk beats the mode; team ForceAsk is advisory.
const SessionPermissionsSource = "session permissions"

// Decide resolves the final permission outcome:
//   - Deny/Allow from any checker win outright.
//   - ForceAsk wins only when it comes from the session layer;
//     team-level ForceAsk falls through so Balanced/Autonomous can
//     override it.
//   - Otherwise applies the (mode × label) table.
func Decide(
	mode session.SafetyPolicy,
	label string,
	checkers []NamedChecker,
	toolName string,
	toolArgs map[string]any,
) PermissionDecision {
	for _, pc := range checkers {
		switch pc.Checker.CheckWithArgs(toolName, toolArgs) {
		case permissions.Deny:
			return PermissionDecision{Outcome: OutcomeDeny, Reason: ReasonChecker, Source: pc.Source}
		case permissions.Allow:
			return PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonChecker, Source: pc.Source}
		case permissions.ForceAsk:
			if pc.Source == SessionPermissionsSource {
				return PermissionDecision{Outcome: OutcomeAsk, Reason: ReasonChecker, Source: pc.Source}
			}
			// Team-level ForceAsk falls through so the mode can override.
		case permissions.Ask:
			// No explicit match at this level; fall through.
		}
	}
	return applyMode(mode, label)
}

// applyMode implements the (mode × label) → verdict table. Empty /
// unrecognised modes fall back to Strict.
//
//	                  safe      destructive     unknown
//	Strict            ask       ask             ask
//	Balanced          allow     ask             ask
//	Autonomous        allow     allow           allow
func applyMode(mode session.SafetyPolicy, label string) PermissionDecision {
	switch mode {
	case session.SafetyPolicyAutonomous:
		return PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonMode, Source: ApprovalSourceModeAutonomous}
	case session.SafetyPolicyBalanced:
		if label == SafetyLabelSafe {
			return PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonMode, Source: ApprovalSourceModeBalanced}
		}
		return PermissionDecision{Outcome: OutcomeAsk, Reason: ReasonMode, Source: ApprovalSourceModeBalanced}
	default:
		return PermissionDecision{Outcome: OutcomeAsk, Reason: ReasonMode, Source: ApprovalSourceModeStrict}
	}
}

// LabelFromAnnotations maps a tool's MCP hints to a safety label:
// DestructiveHint wins, then ReadOnlyHint → safe, else unknown.
func LabelFromAnnotations(a tools.ToolAnnotations) string {
	if a.DestructiveHint != nil && *a.DestructiveHint {
		return SafetyLabelDestructive
	}
	if a.ReadOnlyHint {
		return SafetyLabelSafe
	}
	return SafetyLabelUnknown
}
