package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

// resumeRecordingRuntime records the ResumeRequests forwarded to it.
type resumeRecordingRuntime struct {
	runtime.Runtime

	resumes []runtime.ResumeRequest
}

func (r *resumeRecordingRuntime) Resume(_ context.Context, req runtime.ResumeRequest) {
	r.resumes = append(r.resumes, req)
}

// Legacy resume verbs from older API clients must be normalized before
// they reach the session mutation and the runtime.
func TestResumeSession_NormalizesLegacyVerbs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		confirmation string
		wantType     runtime.ResumeType
		wantPolicy   session.SafetyPolicy
	}{
		{"approve-safe", runtime.ResumeTypeApproveBalanced, session.SafetyPolicyBalanced},
		{"approve-safer", runtime.ResumeTypeApproveBalanced, session.SafetyPolicyBalanced},
		{"approve-session", runtime.ResumeTypeApproveAutonomous, session.SafetyPolicyAutonomous},
		{"approve-balanced", runtime.ResumeTypeApproveBalanced, session.SafetyPolicyBalanced},
		{"approve-autonomous", runtime.ResumeTypeApproveAutonomous, session.SafetyPolicyAutonomous},
	}
	for _, tt := range tests {
		t.Run(tt.confirmation, func(t *testing.T) {
			t.Parallel()
			sess := session.New(session.WithID("s-" + tt.confirmation))
			fake := &resumeRecordingRuntime{}
			sm := newTestSessionManager(t, sess, fake)

			require.NoError(t, sm.ResumeSession(t.Context(), sess.ID, tt.confirmation, "", ""))

			require.Len(t, fake.resumes, 1)
			assert.Equal(t, tt.wantType, fake.resumes[0].Type)
			assert.Equal(t, tt.wantPolicy, sess.GetSafetyPolicy())
		})
	}
}

func TestSetSessionSafetyPolicy_NormalizesAndDowngrades(t *testing.T) {
	t.Parallel()
	sess := session.New(session.WithID("s1"))
	sm := newTestSessionManager(t, sess, &resumeRecordingRuntime{})

	// Legacy alias accepted and normalized.
	require.NoError(t, sm.SetSessionSafetyPolicy(t.Context(), sess.ID, "safe-auto"))
	assert.Equal(t, session.SafetyPolicyBalanced, sess.GetSafetyPolicy())

	// Escalate to autonomous, then downgrade: the legacy ToolsApproved
	// flag must be revoked too, or the downgrade would be ineffective
	// for legacy readers.
	require.NoError(t, sm.SetSessionSafetyPolicy(t.Context(), sess.ID, session.SafetyPolicyAutonomous))
	assert.True(t, sess.IsToolsApproved())

	require.NoError(t, sm.SetSessionSafetyPolicy(t.Context(), sess.ID, session.SafetyPolicyStrict))
	assert.Equal(t, session.SafetyPolicyStrict, sess.GetSafetyPolicy())
	assert.False(t, sess.IsToolsApproved())

	// Restricted is a first-class mode on the API surface.
	require.NoError(t, sm.SetSessionSafetyPolicy(t.Context(), sess.ID, session.SafetyPolicyRestricted))
	assert.Equal(t, session.SafetyPolicyRestricted, sess.GetSafetyPolicy())
	assert.False(t, sess.IsToolsApproved())

	// Invalid values are rejected.
	assert.Error(t, sm.SetSessionSafetyPolicy(t.Context(), sess.ID, "bogus"))
}

// The legacy toggle endpoint round-trips through the safety mode:
// on ⇒ autonomous, off ⇒ back to the legacy default (empty policy,
// read-only auto-approve) rather than explicit strict.
func TestToggleToolApproval_RoundTripsThroughSafetyMode(t *testing.T) {
	t.Parallel()
	sess := session.New(session.WithID("s2"))
	sm := newTestSessionManager(t, sess, &resumeRecordingRuntime{})

	require.NoError(t, sm.ToggleToolApproval(t.Context(), sess.ID))
	assert.Equal(t, session.SafetyPolicyAutonomous, sess.GetSafetyPolicy())
	assert.True(t, sess.IsToolsApproved())

	require.NoError(t, sm.ToggleToolApproval(t.Context(), sess.ID))
	assert.Equal(t, session.SafetyPolicy(""), sess.GetSafetyPolicy())
	assert.False(t, sess.IsToolsApproved())
}

// An explicit balanced choice must survive a toggle round-trip instead
// of being discarded for the legacy default.
func TestToggleToolApproval_RestoresExplicitMode(t *testing.T) {
	t.Parallel()
	sess := session.New(session.WithID("s3"), session.WithSafetyPolicy(session.SafetyPolicyBalanced))
	sm := newTestSessionManager(t, sess, &resumeRecordingRuntime{})

	require.NoError(t, sm.ToggleToolApproval(t.Context(), sess.ID))
	assert.Equal(t, session.SafetyPolicyAutonomous, sess.GetSafetyPolicy())

	require.NoError(t, sm.ToggleToolApproval(t.Context(), sess.ID))
	assert.Equal(t, session.SafetyPolicyBalanced, sess.GetSafetyPolicy())
	assert.False(t, sess.IsToolsApproved())
}
