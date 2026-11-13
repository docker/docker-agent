package toolinstall

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func customToolsDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "custom", "tools")
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", dir)
	return dir
}

func TestToolsDir_Default(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", "")
	assert.Contains(t, ToolsDir(), "tools")
}

func TestToolsDir_EnvOverride(t *testing.T) {
	want := customToolsDir(t)
	assert.Equal(t, filepath.Clean(want), ToolsDir())
}

func TestBinDir(t *testing.T) {
	root := customToolsDir(t)
	assert.Equal(t, filepath.Join(root, "bin"), BinDir())
}

func TestPackageDir(t *testing.T) {
	root := customToolsDir(t)
	assert.Equal(t, filepath.Join(root, "packages", "cli", "cli", "v2.50.0"), PackageDir("cli", "cli", "v2.50.0"))
}

func TestRegistryDir(t *testing.T) {
	root := customToolsDir(t)
	assert.Equal(t, filepath.Join(root, "registry"), RegistryDir())
}

func TestPrependBinDirToEnv_WithExistingPATH(t *testing.T) {
	root := customToolsDir(t)
	existing := filepath.Join(t.TempDir(), "usr", "bin") + string(os.PathListSeparator) + filepath.Join(t.TempDir(), "usr", "local", "bin")
	env := []string{"HOME=" + t.TempDir(), "PATH=" + existing, "FOO=bar"}

	result := PrependBinDirToEnv(env)
	require.Len(t, result, 3)
	assert.Equal(t, "PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+existing, result[1])
}

func TestPrependBinDirToEnv_NoPATH(t *testing.T) {
	root := customToolsDir(t)
	result := PrependBinDirToEnv([]string{"HOME=" + t.TempDir(), "FOO=bar"})
	require.Len(t, result, 3)
	assert.Equal(t, "PATH="+filepath.Join(root, "bin"), result[2])
}

func TestPrependBinDirToEnv_EmptyEnv(t *testing.T) {
	root := customToolsDir(t)
	result := PrependBinDirToEnv(nil)
	require.Len(t, result, 1)
	assert.Equal(t, "PATH="+filepath.Join(root, "bin"), result[0])
}

func TestPrependToPath_CaseInsensitiveKey(t *testing.T) {
	t.Parallel()
	sep := string(os.PathListSeparator)

	tests := []struct {
		name            string
		env             []string
		caseInsensitive bool
		want            []string
	}{
		{
			name:            "windows Path spelling is updated in place",
			env:             []string{"Path=sys", "FOO=bar"},
			caseInsensitive: true,
			want:            []string{"Path=bin" + sep + "sys", "FOO=bar"},
		},
		{
			name:            "case-sensitive platforms treat Path as a different variable",
			env:             []string{"Path=sys"},
			caseInsensitive: false,
			want:            []string{"Path=sys", "PATH=bin"},
		},
		{
			name:            "every spelling is updated so the last one still wins",
			env:             []string{"Path=sys", "PATH=override"},
			caseInsensitive: true,
			want:            []string{"Path=bin" + sep + "sys", "PATH=bin" + sep + "override"},
		},
		{
			name:            "keys that only start with PATH are left alone",
			env:             []string{"PATHEXT=.EXE", "=C:=C:\\"},
			caseInsensitive: true,
			want:            []string{"PATHEXT=.EXE", "=C:=C:\\", "PATH=bin"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, prependToPath(tt.env, "bin", tt.caseInsensitive))
		})
	}
}
