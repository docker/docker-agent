package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSafetyPolicy_IsValid(t *testing.T) {
	t.Parallel()
	cases := map[SafetyPolicy]bool{
		"":                     true,
		SafetyPolicyStrict:     true,
		SafetyPolicyBalanced:   true,
		SafetyPolicyRestricted: true,
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
		SafetyPolicyRestricted: SafetyPolicyRestricted,
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

	s = New(WithSafetyPolicy(SafetyPolicyRestricted))
	assert.Equal(t, SafetyPolicyRestricted, s.SafetyPolicy)
	assert.False(t, s.ToolsApproved, "restricted must not grant blanket approval")

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

// Setting the empty policy is a full reset to the legacy default: the
// blanket approval must be revoked in the same critical section, or a
// concurrent GetSafetyPolicy could still observe autonomous.
func TestSetSafetyPolicy_EmptyResetsToLegacyDefault(t *testing.T) {
	t.Parallel()
	s := New(WithSafetyPolicy(SafetyPolicyAutonomous))
	require.True(t, s.ToolsApproved)

	s.SetSafetyPolicy("")
	assert.Equal(t, SafetyPolicy(""), s.SafetyPolicy)
	assert.False(t, s.ToolsApproved, "reset must revoke the blanket approval")
	assert.Equal(t, SafetyPolicy(""), s.GetSafetyPolicy())
	assert.False(t, s.IsToolsApproved())
}

// The option form treats empty as "no explicit choice" so it composes
// with WithToolsApproved regardless of order (--yolo callers pass
// ToolsApproved=true and an empty policy).
func TestWithSafetyPolicy_EmptyIsNoOp(t *testing.T) {
	t.Parallel()
	s := New(
		WithToolsApproved(true),
		WithSafetyPolicy(""),
	)
	assert.Equal(t, SafetyPolicyAutonomous, s.SafetyPolicy,
		"the --yolo backfill must survive an empty WithSafetyPolicy")
	assert.True(t, s.ToolsApproved)
}

// ToggleYolo must restore the mode that was active before the
// escalation: an explicit balanced/strict choice survives a toggle
// round-trip instead of resetting to the legacy default.
func TestToggleYolo_RestoresPriorMode(t *testing.T) {
	t.Parallel()
	s := New(WithSafetyPolicy(SafetyPolicyBalanced))

	s.ToggleYolo()
	assert.Equal(t, SafetyPolicyAutonomous, s.GetSafetyPolicy())
	assert.True(t, s.IsToolsApproved())

	s.ToggleYolo()
	assert.Equal(t, SafetyPolicyBalanced, s.GetSafetyPolicy())
	assert.False(t, s.IsToolsApproved())
	assert.Equal(t, SafetyPolicy(""), s.PriorSafetyPolicy, "toggle memory must be consumed")
}

// Restricted participates in the yolo toggle like any other explicit
// mode: escalate to autonomous, toggle back restores restricted.
func TestToggleYolo_RestoresRestricted(t *testing.T) {
	t.Parallel()
	s := New(WithSafetyPolicy(SafetyPolicyRestricted))

	s.ToggleYolo()
	assert.Equal(t, SafetyPolicyAutonomous, s.GetSafetyPolicy())
	assert.True(t, s.IsToolsApproved())

	s.ToggleYolo()
	assert.Equal(t, SafetyPolicyRestricted, s.GetSafetyPolicy())
	assert.False(t, s.IsToolsApproved())
}

// A session that never chose a named mode keeps the historical
// contract: toggle-off lands on the legacy default, not strict.
func TestToggleYolo_LegacyRoundTrip(t *testing.T) {
	t.Parallel()
	s := New()

	s.ToggleYolo()
	assert.Equal(t, SafetyPolicyAutonomous, s.GetSafetyPolicy())

	s.ToggleYolo()
	assert.Equal(t, SafetyPolicy(""), s.GetSafetyPolicy())
	assert.False(t, s.ToolsApproved)
}

// A legacy --yolo session (raw flag, no explicit policy) toggles off
// to the legacy default.
func TestToggleYolo_LegacyToolsApprovedSession(t *testing.T) {
	t.Parallel()
	s := New(WithToolsApproved(true))
	require.Equal(t, SafetyPolicyAutonomous, s.GetSafetyPolicy())

	s.ToggleYolo()
	assert.Equal(t, SafetyPolicy(""), s.GetSafetyPolicy())
	assert.False(t, s.ToolsApproved)
}

// An explicit SetSafetyPolicy invalidates the toggle memory: toggling
// off later must not resurrect a mode the user has since replaced.
func TestToggleYolo_ExplicitSetClearsMemory(t *testing.T) {
	t.Parallel()
	s := New(WithSafetyPolicy(SafetyPolicyBalanced))
	s.ToggleYolo()
	require.Equal(t, SafetyPolicyBalanced, s.PriorSafetyPolicy)

	s.SetSafetyPolicy(SafetyPolicyStrict)
	assert.Equal(t, SafetyPolicy(""), s.PriorSafetyPolicy)

	s.ToggleYolo()
	s.ToggleYolo()
	assert.Equal(t, SafetyPolicyStrict, s.GetSafetyPolicy(),
		"round-trip must land on the replacing mode, not the stale one")
}

// ToggleYolo is its own inverse — the server relies on this to roll
// back a toggle whose persistence failed.
func TestToggleYolo_Involution(t *testing.T) {
	t.Parallel()
	s := New(WithSafetyPolicy(SafetyPolicyBalanced))
	s.ToggleYolo() // escalate: autonomous, remembering balanced

	// A failed off-toggle rolled back by toggling again must restore
	// the exact pre-rollback state, memory included.
	s.ToggleYolo()
	s.ToggleYolo()
	assert.Equal(t, SafetyPolicyAutonomous, s.GetSafetyPolicy())
	assert.Equal(t, SafetyPolicyBalanced, s.PriorSafetyPolicy)
}
