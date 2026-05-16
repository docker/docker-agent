package path

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandHomeDir(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "tilde only",
			input:    "~",
			expected: homeDir,
		},
		{
			name:     "expands tilde prefix",
			input:    "~/session.db",
			expected: filepath.Join(homeDir, "session.db"),
		},
		{
			name:     "expands tilde with nested path",
			input:    "~/.cagent/session.db",
			expected: filepath.Join(homeDir, ".cagent", "session.db"),
		},
		{
			name:     "expands tilde with deep path",
			input:    "~/path/to/some/file.db",
			expected: filepath.Join(homeDir, "path", "to", "some", "file.db"),
		},
		{
			name:     "absolute path unchanged",
			input:    "/absolute/path/session.db",
			expected: "/absolute/path/session.db",
		},
		{
			name:     "relative path unchanged",
			input:    "relative/path/session.db",
			expected: "relative/path/session.db",
		},
		{
			name:     "tilde in middle unchanged",
			input:    "/some/~/path/session.db",
			expected: "/some/~/path/session.db",
		},
		{
			name:     "tilde without separator unchanged",
			input:    "~something",
			expected: "~something",
		},
		{
			name:     "tilde slash expands",
			input:    "~/",
			expected: homeDir,
		},
		{
			name:     "empty string unchanged",
			input:    "",
			expected: "",
		},
		{
			name:     "dot path unchanged",
			input:    "./session.db",
			expected: "./session.db",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ExpandHomeDir(tt.input)

			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}
