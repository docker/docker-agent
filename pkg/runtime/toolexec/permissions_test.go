package toolexec

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/safety"
	"github.com/docker/docker-agent/pkg/session"
)

func newChecker(t *testing.T, allow, ask, deny []string) *permissions.Checker {
	t.Helper()
	return permissions.NewChecker(&latest.PermissionsConfig{
		Allow: allow,
		Ask:   ask,
		Deny:  deny,
	})
}

var (
	labelSafeAnnotation = safety.Label{Class: safety.ClassSafe, Origin: safety.OriginAnnotation}
	labelSafeClassifier = safety.Label{Class: safety.ClassSafe, Origin: safety.OriginClassifier}
	labelDestructive    = safety.Label{Class: safety.ClassDestructive, Origin: safety.OriginClassifier}
	labelUnknown        = safety.Label{Class: safety.ClassUnknown, Origin: safety.OriginClassifier}
)

// The full (mode × label) matrix with no custom rules.
func TestDecide_ModeLabelTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mode    session.SafetyPolicy
		label   safety.Label
		outcome PermissionOutcome
	}{
		{"", labelSafeAnnotation, OutcomeAsk}, // legacy allow happens post-hooks in the dispatcher
		{"", labelSafeClassifier, OutcomeAsk},
		{"", labelDestructive, OutcomeAsk},
		{"", labelUnknown, OutcomeAsk},

		{session.SafetyPolicyStrict, labelSafeAnnotation, OutcomeAsk},
		{session.SafetyPolicyStrict, labelSafeClassifier, OutcomeAsk},
		{session.SafetyPolicyStrict, labelDestructive, OutcomeAsk},
		{session.SafetyPolicyStrict, labelUnknown, OutcomeAsk},

		{session.SafetyPolicyBalanced, labelSafeAnnotation, OutcomeAllow},
		{session.SafetyPolicyBalanced, labelSafeClassifier, OutcomeAllow},
		{session.SafetyPolicyBalanced, labelDestructive, OutcomeAsk},
		{session.SafetyPolicyBalanced, labelUnknown, OutcomeAsk},

		// Restricted is fail-closed: safe allows, everything else is
		// denied without prompting.
		{session.SafetyPolicyRestricted, labelSafeAnnotation, OutcomeAllow},
		{session.SafetyPolicyRestricted, labelSafeClassifier, OutcomeAllow},
		{session.SafetyPolicyRestricted, labelDestructive, OutcomeDeny},
		{session.SafetyPolicyRestricted, labelUnknown, OutcomeDeny},

		{session.SafetyPolicyAutonomous, labelSafeAnnotation, OutcomeAllow},
		{session.SafetyPolicyAutonomous, labelSafeClassifier, OutcomeAllow},
		{session.SafetyPolicyAutonomous, labelDestructive, OutcomeAllow},
		{session.SafetyPolicyAutonomous, labelUnknown, OutcomeAllow},

		// Unrecognised modes must behave like strict, never wider.
		{"bogus", labelSafeAnnotation, OutcomeAsk},
		{"bogus", labelDestructive, OutcomeAsk},
	}
	for _, tt := range tests {
		d := Decide(tt.mode, tt.label, nil, "shell", nil)
		assert.Equalf(t, tt.outcome, d.Outcome, "mode=%q label=%s/%s", tt.mode, tt.label.Class, tt.label.Origin)
		assert.Equalf(t, ReasonMode, d.Reason, "mode=%q label=%s", tt.mode, tt.label.Class)
	}
}

// Custom deny rules win over every mode — including Autonomous.
func TestDecide_DenyOverridesAutonomous(t *testing.T) {
	t.Parallel()
	for _, tier := range []Tier{TierSession, TierTeam} {
		d := Decide(session.SafetyPolicyAutonomous, labelSafeClassifier, []NamedChecker{
			{Checker: newChecker(t, nil, nil, []string{"shell"}), Source: "rules", Tier: tier},
		}, "shell", nil)
		assert.Equal(t, PermissionDecision{Outcome: OutcomeDeny, Reason: ReasonChecker, Source: "rules"}, d)
	}
}

// Custom allow rules win over Strict.
func TestDecide_AllowOverridesStrict(t *testing.T) {
	t.Parallel()
	d := Decide(session.SafetyPolicyStrict, labelUnknown, []NamedChecker{
		{Checker: newChecker(t, []string{"read_*"}, nil, nil), Source: "session permissions", Tier: TierSession},
	}, "read_file", nil)
	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonChecker, Source: "session permissions"}, d)
}

// A session-tier ask rule always prompts, even under Autonomous —
// it is direct user intent.
func TestDecide_SessionAskBeatsAutonomous(t *testing.T) {
	t.Parallel()
	d := Decide(session.SafetyPolicyAutonomous, labelSafeClassifier, []NamedChecker{
		{Checker: newChecker(t, nil, []string{"shell"}, nil), Source: "session permissions", Tier: TierSession},
	}, "shell", nil)
	assert.Equal(t, PermissionDecision{Outcome: OutcomeAsk, Reason: ReasonChecker, Source: "session permissions"}, d)
}

// Restricted's mode decisions carry the stable mode_restricted audit
// source on both the allow and the deny leg.
func TestDecide_RestrictedModeSource(t *testing.T) {
	t.Parallel()
	d := Decide(session.SafetyPolicyRestricted, labelSafeClassifier, nil, "shell", nil)
	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonMode, Source: ApprovalSourceModeRestricted}, d)

	d = Decide(session.SafetyPolicyRestricted, labelUnknown, nil, "shell", nil)
	assert.Equal(t, PermissionDecision{Outcome: OutcomeDeny, Reason: ReasonMode, Source: ApprovalSourceModeRestricted}, d)

	d = Decide(session.SafetyPolicyRestricted, labelDestructive, nil, "shell", nil)
	assert.Equal(t, PermissionDecision{Outcome: OutcomeDeny, Reason: ReasonMode, Source: ApprovalSourceModeRestricted}, d)
}

// An explicit allow rule overrides restricted's fail-closed fallback:
// custom rules keep winning over the mode, destructive/unknown included.
func TestDecide_AllowOverridesRestrictedFallback(t *testing.T) {
	t.Parallel()
	for _, tier := range []Tier{TierSession, TierTeam} {
		for _, label := range []safety.Label{labelDestructive, labelUnknown} {
			d := Decide(session.SafetyPolicyRestricted, label, []NamedChecker{
				{Checker: newChecker(t, []string{"shell"}, nil, nil), Source: "rules", Tier: tier},
			}, "shell", nil)
			assert.Equalf(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonChecker, Source: "rules"}, d,
				"tier=%d label=%s", tier, label.Class)
		}
	}
}

// A deny rule still wins under restricted, even for a call the mode
// itself would allow (safe).
func TestDecide_DenyOverridesRestrictedAllow(t *testing.T) {
	t.Parallel()
	d := Decide(session.SafetyPolicyRestricted, labelSafeClassifier, []NamedChecker{
		{Checker: newChecker(t, nil, nil, []string{"shell"}), Source: "rules", Tier: TierTeam},
	}, "shell", nil)
	assert.Equal(t, PermissionDecision{Outcome: OutcomeDeny, Reason: ReasonChecker, Source: "rules"}, d)
}

// A session-tier ask rule is direct user intent and still prompts
// under restricted — the mode only replaces its own fallback.
func TestDecide_SessionAskBeatsRestricted(t *testing.T) {
	t.Parallel()
	for _, label := range []safety.Label{labelSafeClassifier, labelDestructive, labelUnknown} {
		d := Decide(session.SafetyPolicyRestricted, label, []NamedChecker{
			{Checker: newChecker(t, nil, []string{"shell"}, nil), Source: "session permissions", Tier: TierSession},
		}, "shell", nil)
		assert.Equalf(t, PermissionDecision{Outcome: OutcomeAsk, Reason: ReasonChecker, Source: "session permissions"}, d,
			"label=%s", label.Class)
	}
}

// A team-tier ask rule is agent-author advisory: it prompts under
// strict/legacy but yields to a user-chosen auto-deciding mode.
func TestDecide_TeamAskYieldsToAutoDecidingModes(t *testing.T) {
	t.Parallel()
	checkers := []NamedChecker{
		{Checker: newChecker(t, nil, []string{"shell"}, nil), Source: "permissions configuration", Tier: TierTeam},
	}

	d := Decide(session.SafetyPolicyStrict, labelSafeClassifier, checkers, "shell", nil)
	assert.Equal(t, OutcomeAsk, d.Outcome)
	assert.Equal(t, ReasonChecker, d.Reason, "team ask prompts under strict")

	d = Decide("", labelSafeClassifier, checkers, "shell", nil)
	assert.Equal(t, OutcomeAsk, d.Outcome)
	assert.Equal(t, ReasonChecker, d.Reason, "team ask prompts under the legacy default")

	d = Decide(session.SafetyPolicyBalanced, labelSafeClassifier, checkers, "shell", nil)
	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonMode, Source: ApprovalSourceModeBalanced}, d,
		"team ask yields to balanced for safe calls")

	d = Decide(session.SafetyPolicyBalanced, labelDestructive, checkers, "shell", nil)
	assert.Equal(t, OutcomeAsk, d.Outcome, "balanced still asks about destructive calls")

	d = Decide(session.SafetyPolicyAutonomous, labelDestructive, checkers, "shell", nil)
	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonMode, Source: ApprovalSourceYolo}, d,
		"team ask yields to autonomous")

	// Restricted never prompts: a team ask must yield to its mode
	// verdict — allow for safe, deny for destructive/unknown — so the
	// profile stays free of implicit prompts.
	d = Decide(session.SafetyPolicyRestricted, labelSafeClassifier, checkers, "shell", nil)
	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonMode, Source: ApprovalSourceModeRestricted}, d,
		"team ask yields to restricted for safe calls")

	d = Decide(session.SafetyPolicyRestricted, labelDestructive, checkers, "shell", nil)
	assert.Equal(t, PermissionDecision{Outcome: OutcomeDeny, Reason: ReasonMode, Source: ApprovalSourceModeRestricted}, d,
		"team ask yields to restricted's deny for destructive calls")

	d = Decide(session.SafetyPolicyRestricted, labelUnknown, checkers, "shell", nil)
	assert.Equal(t, PermissionDecision{Outcome: OutcomeDeny, Reason: ReasonMode, Source: ApprovalSourceModeRestricted}, d,
		"team ask yields to restricted's deny for unknown calls")
}

func TestDecide_FirstCheckerWins_SessionBeforeTeam(t *testing.T) {
	t.Parallel()
	// Session allows; team denies. Session is checked first → Allow.
	d := Decide(session.SafetyPolicyStrict, labelUnknown, []NamedChecker{
		{Checker: newChecker(t, []string{"shell"}, nil, nil), Source: "session permissions", Tier: TierSession},
		{Checker: newChecker(t, nil, nil, []string{"shell"}), Source: "permissions configuration", Tier: TierTeam},
	}, "shell", nil)

	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonChecker, Source: "session permissions"}, d)
}

func TestDecide_FallsThroughWhenNoCheckerMatches(t *testing.T) {
	t.Parallel()
	// First checker doesn't match anything (no patterns) → falls through to second.
	d := Decide(session.SafetyPolicyStrict, labelUnknown, []NamedChecker{
		{Checker: newChecker(t, nil, nil, nil), Source: "session permissions", Tier: TierSession},
		{Checker: newChecker(t, []string{"shell"}, nil, nil), Source: "permissions configuration", Tier: TierTeam},
	}, "shell", nil)

	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonChecker, Source: "permissions configuration"}, d)
}

func TestDecide_ArgPatternMatching(t *testing.T) {
	t.Parallel()
	// A checker that only allows shell when cmd starts with "ls".
	d := Decide(session.SafetyPolicyStrict, labelSafeClassifier, []NamedChecker{
		{Checker: newChecker(t, []string{"shell:cmd=ls*"}, nil, nil), Source: "session", Tier: TierSession},
	}, "shell", map[string]any{"cmd": "ls -la"})

	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonChecker, Source: "session"}, d)
}

func TestDecide_ArgPatternNoMatchFallsToMode(t *testing.T) {
	t.Parallel()
	d := Decide(session.SafetyPolicyStrict, labelDestructive, []NamedChecker{
		{Checker: newChecker(t, []string{"shell:cmd=ls*"}, nil, nil), Source: "session", Tier: TierSession},
	}, "shell", map[string]any{"cmd": "rm -rf /"})

	assert.Equal(t, OutcomeAsk, d.Outcome)
	assert.Equal(t, ReasonMode, d.Reason)
}

// The legacy read-only fast path applies only to annotation-derived
// safety on sessions that never chose a mode.
func TestLegacyReadOnlyAutoApprove(t *testing.T) {
	t.Parallel()
	assert.True(t, legacyReadOnlyAutoApprove("", labelSafeAnnotation))
	assert.False(t, legacyReadOnlyAutoApprove("", labelSafeClassifier),
		"classifier-safe must not widen the legacy default")
	assert.False(t, legacyReadOnlyAutoApprove("", labelUnknown))
	assert.False(t, legacyReadOnlyAutoApprove(session.SafetyPolicyStrict, labelSafeAnnotation),
		"explicit strict asks for everything")
	assert.False(t, legacyReadOnlyAutoApprove(session.SafetyPolicyBalanced, labelSafeAnnotation),
		"balanced already allows via the mode table")
}
