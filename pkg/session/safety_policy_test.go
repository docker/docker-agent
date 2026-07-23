package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSafetyPolicy_IsValid(t *testing.T) {
	t.Parallel()
	cases := map[SafetyPolicy]bool{
		"":                        true,
		SafetyPolicyStrict:        true,
		SafetyPolicyBalanced:      true,
		SafetyPolicyAutonomous:    true,
		SafetyPolicy("yolo"):      false,
		SafetyPolicy("safer"):     false,
		SafetyPolicy("safe-auto"): false,
		SafetyPolicy("unsafe"):    false,
		SafetyPolicy("Strict"):    false, // case-sensitive on purpose
	}
	for in, want := range cases {
		assert.Equalf(t, want, in.IsValid(), "SafetyPolicy(%q).IsValid()", string(in))
	}
}

// WithSafetyPolicy(autonomous) must flip ToolsApproved=true so legacy
// branches on ToolsApproved still fire. Strict/Balanced intentionally
// leave ToolsApproved alone.
func TestWithSafetyPolicy_AutonomousSyncsToolsApproved(t *testing.T) {
	t.Parallel()
	s := New(WithSafetyPolicy(SafetyPolicyAutonomous))
	assert.Equal(t, SafetyPolicyAutonomous, s.SafetyPolicy)
	assert.True(t, s.ToolsApproved)

	s = New(WithSafetyPolicy(SafetyPolicyBalanced))
	assert.Equal(t, SafetyPolicyBalanced, s.SafetyPolicy)
	assert.False(t, s.ToolsApproved)

	s = New(WithSafetyPolicy(SafetyPolicyStrict))
	assert.Equal(t, SafetyPolicyStrict, s.SafetyPolicy)
	assert.False(t, s.ToolsApproved)
}

// WithToolsApproved(true) must backfill SafetyPolicy=autonomous so
// hooks reading Input.SafetyPolicy see the correct value for legacy
// --yolo callers that haven't migrated.
func TestWithToolsApproved_BackfillsSafetyPolicy(t *testing.T) {
	t.Parallel()
	s := New(WithToolsApproved(true))
	assert.True(t, s.ToolsApproved)
	assert.Equal(t, SafetyPolicyAutonomous, s.SafetyPolicy)

	s = New(WithToolsApproved(false))
	assert.False(t, s.ToolsApproved)
	assert.Equal(t, SafetyPolicy(""), s.SafetyPolicy)
}

// Explicit WithSafetyPolicy after WithToolsApproved wins over the
// backfill (e.g. yolo + balanced would be a rare combination).
func TestWithSafetyPolicy_ExplicitWinsOverToolsApproved(t *testing.T) {
	t.Parallel()
	s := New(
		WithToolsApproved(true),
		WithSafetyPolicy(SafetyPolicyBalanced),
	)
	assert.True(t, s.ToolsApproved)
	assert.Equal(t, SafetyPolicyBalanced, s.SafetyPolicy)
}

// SetSafetyPolicy mid-session mirrors WithSafetyPolicy: setting
// autonomous backfills ToolsApproved; other modes leave it alone.
// Used by the dispatcher's approve-balanced / approve-autonomous
// resume handlers to opt an existing session into a new mode.
func TestSetSafetyPolicy_MidSession(t *testing.T) {
	t.Parallel()
	s := New()
	assert.Equal(t, SafetyPolicy(""), s.SafetyPolicy)
	assert.False(t, s.ToolsApproved)

	s.SetSafetyPolicy(SafetyPolicyBalanced)
	assert.Equal(t, SafetyPolicyBalanced, s.SafetyPolicy)
	assert.False(t, s.ToolsApproved, "balanced must not backfill ToolsApproved")

	s.SetSafetyPolicy(SafetyPolicyAutonomous)
	assert.Equal(t, SafetyPolicyAutonomous, s.SafetyPolicy)
	assert.True(t, s.ToolsApproved, "autonomous must backfill ToolsApproved for legacy branches")

	s.SetSafetyPolicy(SafetyPolicyStrict)
	assert.Equal(t, SafetyPolicyStrict, s.SafetyPolicy)
	assert.True(t, s.ToolsApproved, "SetSafetyPolicy does not un-flip ToolsApproved")
}
