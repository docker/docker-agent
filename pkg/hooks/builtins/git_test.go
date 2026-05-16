package builtins

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsGitRepo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T) string
		want  bool
	}{
		{
			name: "directory containing .git/",
			setup: func(t *testing.T) string {
				t.Helper()
				dir := t.TempDir()
				require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
				return dir
			},
			want: true,
		},
		{
			name: "subdirectory of a git repo",
			setup: func(t *testing.T) string {
				t.Helper()
				parent := t.TempDir()
				require.NoError(t, os.Mkdir(filepath.Join(parent, ".git"), 0o755))
				child := filepath.Join(parent, "child")
				require.NoError(t, os.Mkdir(child, 0o755))
				return child
			},
			want: true,
		},
		{
			name: ".git is a regular file, not a directory",
			setup: func(t *testing.T) string {
				t.Helper()
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".git"), nil, 0o644))
				return dir
			},
			want: false,
		},
		{
			name:  "directory with no .git anywhere up the tree",
			setup: func(t *testing.T) string { t.Helper(); return t.TempDir() },
			want:  false,
		},
		{
			name:  "nonexistent directory",
			setup: func(*testing.T) string { return "/path/that/does/not/exist" },
			want:  false,
		},
		{
			name:  "empty path",
			setup: func(*testing.T) string { return "" },
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isGitRepo(tt.setup(t)))
		})
	}
}
