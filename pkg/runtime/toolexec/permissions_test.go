package toolexec

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func newChecker(t *testing.T, allow, ask, deny []string) *permissions.Checker {
	t.Helper()
	return permissions.NewChecker(&latest.PermissionsConfig{
		Allow: allow,
		Ask:   ask,
		Deny:  deny,
	})
}

// Custom deny beats every mode, including Autonomous. This is the
// hardest guarantee custom rules give the user.
func TestDecide_CustomDenyBeatsAutonomous(t *testing.T) {
	t.Parallel()
	d := Decide(session.SafetyPolicyAutonomous, SafetyLabelSafe, []NamedChecker{
		{Checker: newChecker(t, nil, nil, []string{"shell"}), Source: "team"},
	}, "shell", nil)

	assert.Equal(t, PermissionDecision{Outcome: OutcomeDeny, Reason: ReasonChecker, Source: "team"}, d)
}

// Custom allow beats the safety mode's Ask under Strict/Balanced.
func TestDecide_CustomAllowBeatsMode(t *testing.T) {
	t.Parallel()
	d := Decide(session.SafetyPolicyStrict, SafetyLabelDestructive, []NamedChecker{
		{Checker: newChecker(t, []string{"shell"}, nil, nil), Source: "session"},
	}, "shell", nil)

	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonChecker, Source: "session"}, d)
}

// Session-level ask beats Autonomous. Users who add explicit ask
// patterns via the Custom rules UI get their targeted prompt even
// under yolo.
func TestDecide_SessionAskBeatsAutonomous(t *testing.T) {
	t.Parallel()
	d := Decide(session.SafetyPolicyAutonomous, SafetyLabelSafe, []NamedChecker{
		{Checker: newChecker(t, nil, []string{"shell:cmd=git push --force*"}, nil), Source: SessionPermissionsSource},
	}, "shell", map[string]any{"cmd": "git push --force origin main"})

	assert.Equal(t, PermissionDecision{Outcome: OutcomeAsk, Reason: ReasonChecker, Source: SessionPermissionsSource}, d)
}

// Team-level ask (e.g. `ask: "*"` in an agent YAML) is advisory: it
// forces asking only when the mode itself would ask. Under Balanced
// with a classifier-safe tool, the mode overrides and auto-approves
// so YAML wildcards don't defeat the user's mode choice.
func TestDecide_TeamAskFallsThroughToMode(t *testing.T) {
	t.Parallel()
	d := Decide(session.SafetyPolicyBalanced, SafetyLabelSafe, []NamedChecker{
		{Checker: newChecker(t, nil, []string{"*"}, nil), Source: "permissions configuration"},
	}, "read_multiple_files", nil)

	assert.Equal(t, PermissionDecision{
		Outcome: OutcomeAllow, Reason: ReasonMode, Source: "mode_balanced",
	}, d, "team-level ask: * must not defeat Balanced for classifier-safe tools")
}

// First matching checker wins; session (checked first) overrides team.
func TestDecide_FirstCheckerWins_SessionBeforeTeam(t *testing.T) {
	t.Parallel()
	d := Decide(session.SafetyPolicyStrict, SafetyLabelUnknown, []NamedChecker{
		{Checker: newChecker(t, []string{"shell"}, nil, nil), Source: "session permissions"},
		{Checker: newChecker(t, nil, nil, []string{"shell"}), Source: "permissions configuration"},
	}, "shell", nil)

	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonChecker, Source: "session permissions"}, d)
}

// Empty first checker falls through to the second.
func TestDecide_FallsThroughWhenNoCheckerMatches(t *testing.T) {
	t.Parallel()
	d := Decide(session.SafetyPolicyStrict, SafetyLabelUnknown, []NamedChecker{
		{Checker: newChecker(t, nil, nil, nil), Source: "session permissions"},
		{Checker: newChecker(t, []string{"shell"}, nil, nil), Source: "permissions configuration"},
	}, "shell", nil)

	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonChecker, Source: "permissions configuration"}, d)
}

// Arg-pattern matching is unchanged: `shell:cmd=ls*` matches `ls -la`.
func TestDecide_ArgPatternMatching(t *testing.T) {
	t.Parallel()
	d := Decide(session.SafetyPolicyStrict, SafetyLabelSafe, []NamedChecker{
		{Checker: newChecker(t, []string{"shell:cmd=ls*"}, nil, nil), Source: "session"},
	}, "shell", map[string]any{"cmd": "ls -la"})

	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonChecker, Source: "session"}, d)
}

// Arg-pattern non-match falls through to the mode table.
func TestDecide_ArgPatternNoMatchFallsToMode(t *testing.T) {
	t.Parallel()
	d := Decide(session.SafetyPolicyStrict, SafetyLabelSafe, []NamedChecker{
		{Checker: newChecker(t, []string{"shell:cmd=ls*"}, nil, nil), Source: "session"},
	}, "shell", map[string]any{"cmd": "rm -rf /"})

	assert.Equal(t, PermissionDecision{Outcome: OutcomeAsk, Reason: ReasonMode, Source: "mode_strict"}, d)
}

// TestDecide_ModeVerdictTable pins the (mode × label) → verdict
// table. Every cell is exercised because a subtle regression here
// would silently either over-approve or over-prompt.
func TestDecide_ModeVerdictTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode        session.SafetyPolicy
		label       string
		wantOutcome PermissionOutcome
		wantSource  string
	}{
		{session.SafetyPolicyStrict, SafetyLabelSafe, OutcomeAsk, "mode_strict"},
		{session.SafetyPolicyStrict, SafetyLabelDestructive, OutcomeAsk, "mode_strict"},
		{session.SafetyPolicyStrict, SafetyLabelUnknown, OutcomeAsk, "mode_strict"},
		{session.SafetyPolicyBalanced, SafetyLabelSafe, OutcomeAllow, "mode_balanced"},
		{session.SafetyPolicyBalanced, SafetyLabelDestructive, OutcomeAsk, "mode_balanced"},
		{session.SafetyPolicyBalanced, SafetyLabelUnknown, OutcomeAsk, "mode_balanced"},
		{session.SafetyPolicyAutonomous, SafetyLabelSafe, OutcomeAllow, "mode_autonomous"},
		{session.SafetyPolicyAutonomous, SafetyLabelDestructive, OutcomeAllow, "mode_autonomous"},
		{session.SafetyPolicyAutonomous, SafetyLabelUnknown, OutcomeAllow, "mode_autonomous"},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode)+"_"+tc.label, func(t *testing.T) {
			t.Parallel()
			d := Decide(tc.mode, tc.label, nil, "some_tool", nil)
			assert.Equal(t, tc.wantOutcome, d.Outcome)
			assert.Equal(t, ReasonMode, d.Reason)
			assert.Equal(t, tc.wantSource, d.Source)
		})
	}
}

// Empty mode is treated as Strict — the safe default when no session
// preference has been recorded.
func TestDecide_EmptyModeDefaultsToStrict(t *testing.T) {
	t.Parallel()
	d := Decide("", SafetyLabelSafe, nil, "read_file", nil)
	assert.Equal(t, PermissionDecision{Outcome: OutcomeAsk, Reason: ReasonMode, Source: "mode_strict"}, d)
}

// LabelFromAnnotations: DestructiveHint wins, then ReadOnlyHint →
// safe, else unknown.
func TestLabelFromAnnotations(t *testing.T) {
	t.Parallel()
	trueVal := true
	falseVal := false
	cases := []struct {
		name string
		in   tools.ToolAnnotations
		want string
	}{
		{"destructive beats read-only", tools.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &trueVal}, SafetyLabelDestructive},
		{"destructive alone", tools.ToolAnnotations{DestructiveHint: &trueVal}, SafetyLabelDestructive},
		{"read-only, destructive=false", tools.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &falseVal}, SafetyLabelSafe},
		{"read-only, destructive nil", tools.ToolAnnotations{ReadOnlyHint: true}, SafetyLabelSafe},
		{"neither", tools.ToolAnnotations{}, SafetyLabelUnknown},
		{"destructive=false only", tools.ToolAnnotations{DestructiveHint: &falseVal}, SafetyLabelUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, LabelFromAnnotations(tc.in))
		})
	}
}
