package toolexec

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/permissions"
)

func newChecker(t *testing.T, allow, ask, deny []string) *permissions.Checker {
	t.Helper()
	return permissions.NewChecker(&latest.PermissionsConfig{
		Allow: allow,
		Ask:   ask,
		Deny:  deny,
	})
}

func TestDecide_YoloShortCircuits(t *testing.T) {
	d := Decide(true, []NamedChecker{
		{Checker: newChecker(t, nil, nil, []string{"shell"}), Source: "team"},
	}, "shell", nil, false)

	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonYolo}, d)
}

func TestDecide_DenyFromCheckerWins(t *testing.T) {
	d := Decide(false, []NamedChecker{
		{Checker: newChecker(t, nil, nil, []string{"shell"}), Source: "session"},
	}, "shell", nil, true /* read-only doesn't bypass deny */)

	assert.Equal(t, PermissionDecision{Outcome: OutcomeDeny, Reason: ReasonChecker, Source: "session"}, d)
}

func TestDecide_AllowFromChecker(t *testing.T) {
	d := Decide(false, []NamedChecker{
		{Checker: newChecker(t, []string{"read_*"}, nil, nil), Source: "session"},
	}, "read_file", nil, false)

	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonChecker, Source: "session"}, d)
}

func TestDecide_ForceAskFromCheckerOverridesReadOnly(t *testing.T) {
	d := Decide(false, []NamedChecker{
		{Checker: newChecker(t, nil, []string{"read_file"}, nil), Source: "team"},
	}, "read_file", nil, true /* read-only would normally bypass ask */)

	assert.Equal(t, PermissionDecision{Outcome: OutcomeAsk, Reason: ReasonChecker, Source: "team"}, d)
}

func TestDecide_FirstCheckerWins_SessionBeforeTeam(t *testing.T) {
	// Session allows; team denies. Session is checked first → Allow.
	d := Decide(false, []NamedChecker{
		{Checker: newChecker(t, []string{"shell"}, nil, nil), Source: "session permissions"},
		{Checker: newChecker(t, nil, nil, []string{"shell"}), Source: "permissions configuration"},
	}, "shell", nil, false)

	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonChecker, Source: "session permissions"}, d)
}

func TestDecide_FallsThroughWhenNoCheckerMatches(t *testing.T) {
	// First checker doesn't match anything (no patterns) → falls through to second.
	d := Decide(false, []NamedChecker{
		{Checker: newChecker(t, nil, nil, nil), Source: "session permissions"},
		{Checker: newChecker(t, []string{"shell"}, nil, nil), Source: "permissions configuration"},
	}, "shell", nil, false)

	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonChecker, Source: "permissions configuration"}, d)
}

func TestDecide_ReadOnlyHintAutoApproves(t *testing.T) {
	d := Decide(false, nil, "read_file", nil, true)
	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonReadOnlyHint}, d)
}

func TestDecide_DefaultAsk(t *testing.T) {
	d := Decide(false, nil, "shell", nil, false)
	assert.Equal(t, PermissionDecision{Outcome: OutcomeAsk, Reason: ReasonDefault}, d)
}

func TestDecide_NoCheckersWithReadOnly(t *testing.T) {
	d := Decide(false, []NamedChecker{}, "read_file", nil, true)
	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonReadOnlyHint}, d)
}

func TestDecide_ArgPatternMatching(t *testing.T) {
	// A checker that only allows shell when cmd starts with "ls".
	d := Decide(false, []NamedChecker{
		{Checker: newChecker(t, []string{"shell:cmd=ls*"}, nil, nil), Source: "session"},
	}, "shell", map[string]any{"cmd": "ls -la"}, false)

	assert.Equal(t, PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonChecker, Source: "session"}, d)
}

func TestDecide_ArgPatternNoMatchFallsToDefault(t *testing.T) {
	d := Decide(false, []NamedChecker{
		{Checker: newChecker(t, []string{"shell:cmd=ls*"}, nil, nil), Source: "session"},
	}, "shell", map[string]any{"cmd": "rm -rf /"}, false)

	assert.Equal(t, PermissionDecision{Outcome: OutcomeAsk, Reason: ReasonDefault}, d)
}
