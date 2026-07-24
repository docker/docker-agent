package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsValidResumeType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   ResumeType
		want bool
	}{
		{"approve", ResumeTypeApprove, true},
		{"approve-balanced", ResumeTypeApproveBalanced, true},
		{"approve-autonomous", ResumeTypeApproveAutonomous, true},
		{"approve-tool", ResumeTypeApproveTool, true},
		{"reject", ResumeTypeReject, true},
		// Legacy verbs stay accepted for older clients.
		{"approve-session", ResumeTypeApproveSession, true},
		{"approve-safe", ResumeTypeApproveSafe, true},
		{"approve-safer", ResumeTypeApproveSafer, true},
		{"empty", ResumeType(""), false},
		{"unknown", ResumeType("yolo"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, IsValidResumeType(tt.in))
		})
	}
}

func TestNormalizeResumeType(t *testing.T) {
	t.Parallel()

	cases := map[ResumeType]ResumeType{
		ResumeTypeApprove:           ResumeTypeApprove,
		ResumeTypeApproveBalanced:   ResumeTypeApproveBalanced,
		ResumeTypeApproveAutonomous: ResumeTypeApproveAutonomous,
		ResumeTypeApproveTool:       ResumeTypeApproveTool,
		ResumeTypeReject:            ResumeTypeReject,
		ResumeTypeApproveSession:    ResumeTypeApproveAutonomous,
		ResumeTypeApproveSafe:       ResumeTypeApproveBalanced,
		ResumeTypeApproveSafer:      ResumeTypeApproveBalanced,
		ResumeType("bogus"):         ResumeType("bogus"),
	}
	for in, want := range cases {
		assert.Equalf(t, want, NormalizeResumeType(in), "NormalizeResumeType(%q)", in)
	}
}

func TestValidResumeTypes(t *testing.T) {
	t.Parallel()

	got := ValidResumeTypes()

	// Every returned type must round-trip through IsValidResumeType.
	for _, rt := range got {
		assert.Truef(t, IsValidResumeType(rt), "ValidResumeTypes() returned %q which IsValidResumeType rejects", rt)
	}

	assert.ElementsMatch(t, []ResumeType{
		ResumeTypeApprove,
		ResumeTypeApproveBalanced,
		ResumeTypeApproveAutonomous,
		ResumeTypeApproveTool,
		ResumeTypeReject,
	}, got)
}

func TestResumeApproveHelpers(t *testing.T) {
	t.Parallel()

	t.Run("approve", func(t *testing.T) {
		t.Parallel()
		r := ResumeApprove()
		assert.Equal(t, ResumeTypeApprove, r.Type)
		assert.Empty(t, r.Reason)
		assert.Empty(t, r.ToolName)
	})

	t.Run("approve-balanced", func(t *testing.T) {
		t.Parallel()
		r := ResumeApproveBalanced()
		assert.Equal(t, ResumeTypeApproveBalanced, r.Type)
		assert.Empty(t, r.Reason)
		assert.Empty(t, r.ToolName)
	})

	t.Run("approve-autonomous", func(t *testing.T) {
		t.Parallel()
		r := ResumeApproveAutonomous()
		assert.Equal(t, ResumeTypeApproveAutonomous, r.Type)
		assert.Empty(t, r.Reason)
		assert.Empty(t, r.ToolName)
	})

	t.Run("approve-session is a deprecated alias for autonomous", func(t *testing.T) {
		t.Parallel()
		r := ResumeApproveSession()
		assert.Equal(t, ResumeTypeApproveAutonomous, r.Type)
	})

	t.Run("approve-tool", func(t *testing.T) {
		t.Parallel()
		r := ResumeApproveTool("read_file")
		assert.Equal(t, ResumeTypeApproveTool, r.Type)
		assert.Equal(t, "read_file", r.ToolName)
		assert.Empty(t, r.Reason)
	})

	t.Run("approve-tool-empty", func(t *testing.T) {
		t.Parallel()
		// Empty tool name is allowed at the constructor level; validation
		// happens when the request is consumed by the runtime.
		r := ResumeApproveTool("")
		assert.Equal(t, ResumeTypeApproveTool, r.Type)
		assert.Empty(t, r.ToolName)
	})

	t.Run("reject-with-reason", func(t *testing.T) {
		t.Parallel()
		r := ResumeReject("dangerous command")
		assert.Equal(t, ResumeTypeReject, r.Type)
		assert.Equal(t, "dangerous command", r.Reason)
	})

	t.Run("reject-empty-reason", func(t *testing.T) {
		t.Parallel()
		r := ResumeReject("")
		assert.Equal(t, ResumeTypeReject, r.Type)
		assert.Empty(t, r.Reason)
	})
}
