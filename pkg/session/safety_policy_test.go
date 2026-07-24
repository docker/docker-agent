package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSafetyPolicy_IsValid(t *testing.T) {
	t.Parallel()
	cases := map[SafetyPolicy]bool{
		"":                     true,
		SafetyPolicyStrict:     true,
		SafetyPolicyBalanced:   true,
		SafetyPolicyAutonomous: true,
		// Legacy aliases stay accepted on input.
		"unsafe":              true,
		"safer":               true,
		"safe-auto":           true,
		SafetyPolicy("yolo"):  false,
		SafetyPolicy("Safer"): false, // case-sensitive on purpose
	}
	for in, want := range cases {
		assert.Equalf(t, want, in.IsValid(), "SafetyPolicy(%q).IsValid()", string(in))
	}
}

func TestSafetyPolicy_Normalize(t *testing.T) {
	t.Parallel()
	cases := map[SafetyPolicy]SafetyPolicy{
		"":                     "",
		SafetyPolicyStrict:     SafetyPolicyStrict,
		SafetyPolicyBalanced:   SafetyPolicyBalanced,
		SafetyPolicyAutonomous: SafetyPolicyAutonomous,
		"unsafe":               SafetyPolicyAutonomous,
		"safer":                SafetyPolicyBalanced,
		"safe-auto":            SafetyPolicyBalanced,
		// Unrecognised values collapse to strict — an invalid input
		// must never widen approval.
		"bogus": SafetyPolicyStrict,
	}
	for in, want := range cases {
		assert.Equalf(t, want, in.Normalize(), "SafetyPolicy(%q).Normalize()", string(in))
	}
}

// WithSafetyPolicy keeps ToolsApproved in sync both ways so legacy
// readers of the flag always agree with the mode.
func TestWithSafetyPolicy_SyncsToolsApproved(t *testing.T) {
	t.Parallel()
	s := New(WithSafetyPolicy(SafetyPolicyAutonomous))
	assert.Equal(t, SafetyPolicyAutonomous, s.SafetyPolicy)
	assert.True(t, s.ToolsApproved)

	s = New(WithSafetyPolicy(SafetyPolicyBalanced))
	assert.Equal(t, SafetyPolicyBalanced, s.SafetyPolicy)
	assert.False(t, s.ToolsApproved)

	// Legacy alias normalizes at the boundary.
	s = New(WithSafetyPolicy("unsafe"))
	assert.Equal(t, SafetyPolicyAutonomous, s.SafetyPolicy)
	assert.True(t, s.ToolsApproved)
}

// WithToolsApproved(true) must backfill SafetyPolicy=autonomous so hooks
// reading Input.SafetyPolicy see the correct value for legacy --yolo
// callers that haven't migrated.
func TestWithToolsApproved_BackfillsSafetyPolicy(t *testing.T) {
	t.Parallel()
	s := New(WithToolsApproved(true))
	assert.True(t, s.ToolsApproved)
	assert.Equal(t, SafetyPolicyAutonomous, s.SafetyPolicy)

	s = New(WithToolsApproved(false))
	assert.False(t, s.ToolsApproved)
	assert.Equal(t, SafetyPolicy(""), s.SafetyPolicy)
}

// GetSafetyPolicy is the single source of truth: it normalizes legacy
// values and upgrades a bare ToolsApproved flag to Autonomous.
func TestGetSafetyPolicy(t *testing.T) {
	t.Parallel()
	s := New()
	assert.Equal(t, SafetyPolicy(""), s.GetSafetyPolicy())

	// Simulate a legacy persisted session: raw field writes.
	s = New()
	s.ToolsApproved = true
	assert.Equal(t, SafetyPolicyAutonomous, s.GetSafetyPolicy())
	assert.True(t, s.IsToolsApproved())

	s = New()
	s.SafetyPolicy = "safe-auto"
	assert.Equal(t, SafetyPolicyBalanced, s.GetSafetyPolicy())
	assert.False(t, s.IsToolsApproved())
}

// SetSafetyPolicy syncs ToolsApproved both ways so a mode downgrade
// genuinely revokes the blanket approval. Used by the dispatcher's
// approve-balanced / approve-autonomous resume handlers and the
// safety-policy API endpoint.
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
	assert.True(t, s.ToolsApproved, "autonomous must backfill ToolsApproved for legacy readers")

	s.SetSafetyPolicy(SafetyPolicyStrict)
	assert.Equal(t, SafetyPolicyStrict, s.SafetyPolicy)
	assert.False(t, s.ToolsApproved, "downgrading from autonomous must revoke the blanket approval")
	assert.False(t, s.IsToolsApproved())
}
