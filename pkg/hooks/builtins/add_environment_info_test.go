package builtins

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvironmentInfo(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func() string
		expectGit bool
	}{
		{
			name: "with git repo",
			setupFunc: func() string {
				tmpDir := t.TempDir()
				require.NoError(t, os.Mkdir(filepath.Join(tmpDir, ".git"), 0o755))
				return tmpDir
			},
			expectGit: true,
		},
		{
			name:      "without git repo",
			setupFunc: t.TempDir,
		},
		{
			name:      "nonexistent directory",
			setupFunc: func() string { return "/path/that/does/not/exist" },
		},
		{
			name:      "empty directory path",
			setupFunc: func() string { return "" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := tt.setupFunc()

			gitStatus := "No"
			if tt.expectGit {
				gitStatus = "Yes"
			}
			expected := `Here is useful information about the environment you are running in:
	<env>
	Working directory: ` + dir + `
	Is directory a git repo: ` + gitStatus + `
	Operating System: ` + displayOS() + `
	CPU Architecture: ` + displayArch() + `
	</env>`

			assert.Equal(t, expected, environmentInfo(dir))
		})
	}
}
