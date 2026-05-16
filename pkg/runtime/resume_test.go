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
		{"approve-session", ResumeTypeApproveSession, true},
		{"approve-tool", ResumeTypeApproveTool, true},
		{"reject", ResumeTypeReject, true},
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

func TestValidResumeTypes(t *testing.T) {
	t.Parallel()

	got := ValidResumeTypes()

	// Every returned type must round-trip through IsValidResumeType.
	for _, rt := range got {
		assert.Truef(t, IsValidResumeType(rt), "ValidResumeTypes() returned %q which IsValidResumeType rejects", rt)
	}

	// And the canonical four must all be present.
	assert.ElementsMatch(t, []ResumeType{
		ResumeTypeApprove,
		ResumeTypeApproveSession,
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

	t.Run("approve-session", func(t *testing.T) {
		t.Parallel()
		r := ResumeApproveSession()
		assert.Equal(t, ResumeTypeApproveSession, r.Type)
		assert.Empty(t, r.Reason)
		assert.Empty(t, r.ToolName)
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
