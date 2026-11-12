package evaluation

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Capture the fixture path before tests change the process working directory.
var mockCLILauncher, mockCLILauncherErr = filepath.Abs("testdata/cli")

func writeMockCLI(t *testing.T, path, script string) {
	t.Helper()
	require.NoError(t, mockCLILauncherErr)
	require.NoError(t, os.WriteFile(path+".script", []byte(script), 0o600))
	require.NoError(t, os.Symlink(mockCLILauncher, path))
}

func TestMockCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	dir := filepath.Join(t.TempDir(), "space ' $dir")
	require.NoError(t, os.Mkdir(dir, 0o700))
	first := filepath.Join(dir, "docker")
	second := filepath.Join(dir, "sbx")
	writeMockCLI(t, first, `printf '<%s>\n' "$@"; printf diagnostic >&2; exit 7`)
	writeMockCLI(t, second, `exec /bin/sh -c 'printf alternate; exit 9'`)
	a, err := os.Stat(first)
	require.NoError(t, err)
	b, err := os.Stat(second)
	require.NoError(t, err)
	assert.True(t, os.SameFile(a, b))
	t.Chdir(t.TempDir())
	cmd := exec.CommandContext(t.Context(), first, "", "two words", "$(echo literal)")
	cmd.Env = []string{"PATH=" + dir}
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 7, exitErr.ExitCode())
	assert.Equal(t, "<>\n<two words>\n<$(echo literal)>\ndiagnostic", string(out))
	cmd = exec.CommandContext(t.Context(), second)
	cmd.Env = []string{"PATH=" + dir}
	out, err = cmd.Output()
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 9, exitErr.ExitCode())
	assert.Equal(t, "alternate", string(out))
}
