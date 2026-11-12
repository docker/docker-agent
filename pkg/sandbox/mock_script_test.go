package sandbox_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMockScriptsShareExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	dir := filepath.Join(t.TempDir(), "space ' $dir")
	require.NoError(t, os.Mkdir(dir, 0o700))
	writeMockScript(t, dir, "docker", `printf '<%s>\n' "$@"
printf 'diagnostic' >&2
(exit 7)`)
	writeMockScript(t, dir, "sbx", `exec /bin/sh -c 'printf alternate; exit 9'`)
	docker := filepath.Join(dir, "docker")
	sbx := filepath.Join(dir, "sbx")
	first, err := os.Stat(docker)
	require.NoError(t, err)
	second, err := os.Stat(sbx)
	require.NoError(t, err)
	assert.True(t, os.SameFile(first, second))

	cmd := exec.CommandContext(t.Context(), docker, "", "two words", "$(echo literal)")
	cmd.Env = []string{"PATH=" + dir}
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 7, exitErr.ExitCode())
	assert.Equal(t, "<>\n<two words>\n<$(echo literal)>\ndiagnostic", string(output))

	cmd = exec.CommandContext(t.Context(), sbx)
	cmd.Env = []string{"PATH=" + dir}
	output, err = cmd.Output()
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 9, exitErr.ExitCode())
	assert.Equal(t, "alternate", string(output))
}
