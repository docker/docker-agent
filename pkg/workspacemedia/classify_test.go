package workspacemedia

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassifyRequestedPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		requested string
		class     PathClass
		cleaned   string
	}{
		{"cat.png", PathWorkspaceRelative, "cat.png"},
		{"images/cat.png", PathWorkspaceRelative, "images/cat.png"},
		{`images\cat.png`, PathWorkspaceRelative, "images/cat.png"},
		{"./images/cat.png", PathWorkspaceRelative, "images/cat.png"},
		{"a/../b.png", PathWorkspaceRelative, "b.png"},
		{"a/b/../../c.png", PathWorkspaceRelative, "c.png"},

		{"/abs/cat.png", PathEscaping, ""},
		{`\abs\cat.png`, PathEscaping, ""},
		{`C:\abs\cat.png`, PathEscaping, ""},
		{"../cat.png", PathEscaping, ""},
		{"a/../../cat.png", PathEscaping, ""},
		{"~/cat.png", PathEscaping, ""},
		{"~", PathEscaping, ""},
		{"~user/cat.png", PathEscaping, ""},

		{"//", PathEscaping, ""},

		{"", PathInvalid, ""},
		{".", PathInvalid, ""},
		{"a/..", PathInvalid, ""},
		{"CON.png", PathInvalid, ""},
		{"...", PathInvalid, ""},
	}
	for _, tt := range tests {
		class, cleaned := ClassifyRequestedPath(tt.requested)
		assert.Equal(t, tt.class, class, "class of %q", tt.requested)
		assert.Equal(t, tt.cleaned, cleaned, "cleaned form of %q", tt.requested)
	}
}

func TestRequestedBasename(t *testing.T) {
	t.Parallel()
	tests := []struct {
		requested string
		want      string
	}{
		{"cat.png", "cat.png"},
		{"/abs/dir/cat.png", "cat.png"},
		{"../outside/cat.png", "cat.png"},
		{`C:\dir\cat.png`, "cat.png"},
		{"dir/name/..", "name"},
		{"", ""},
		{"/", ""},
		{"../..", ""},
		{"./.", ""},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, RequestedBasename(tt.requested), "basename of %q", tt.requested)
	}
}
