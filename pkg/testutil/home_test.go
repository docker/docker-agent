package testutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/paths"
)

// useHome gives the test a known home, as if no suite had isolated it yet.
func useHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(tempHomeEnv, "")
	return home
}

func userHomeDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	return home
}

func TestRunWithTempHome(t *testing.T) {
	original := useHome(t)
	config := filepath.Join(original, ".config", "cagent", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(config), 0o700))
	require.NoError(t, os.WriteFile(config, []byte("settings:\n  lean: true\n"), 0o600))

	var home string
	code := RunWithTempHome(func() int {
		home = userHomeDir(t)
		assert.NotEqual(t, original, home)

		entries, err := os.ReadDir(home)
		require.NoError(t, err)
		assert.Empty(t, entries)

		assert.Equal(t, filepath.Join(home, ".config", "cagent"), paths.GetConfigDir())
		assert.Equal(t, filepath.Join(home, ".cagent"), paths.GetDataDir())

		// pkg/path prefers HOME over os.UserHomeDir on every platform.
		expanded, err := pathx.ExpandHomeDir("~")
		require.NoError(t, err)
		assert.Equal(t, home, expanded)

		require.NoError(t, os.WriteFile(filepath.Join(home, "leftover"), nil, 0o600))
		return 23
	})

	assert.Equal(t, 23, code)
	assert.Equal(t, original, os.Getenv("HOME"))
	assert.Equal(t, original, os.Getenv("USERPROFILE"))
	assert.Empty(t, os.Getenv(tempHomeEnv))
	assert.NoDirExists(t, home)
	assert.FileExists(t, config)
}

func TestRunWithTempHomeCleansUpAfterPanic(t *testing.T) {
	original := useHome(t)

	var home string
	assert.Panics(t, func() {
		RunWithTempHome(func() int {
			home = userHomeDir(t)
			panic("boom")
		})
	})

	assert.Equal(t, original, os.Getenv("HOME"))
	assert.Equal(t, original, os.Getenv("USERPROFILE"))
	assert.Empty(t, os.Getenv(tempHomeEnv))
	assert.NoDirExists(t, home)
}

// A re-executed test binary inherits the environment of the test that starts
// it, including a home that test may have chosen on purpose.
func TestRunWithTempHomeNestedRunKeepsInheritedHome(t *testing.T) {
	useHome(t)

	RunWithTempHome(func() int {
		suiteHome := userHomeDir(t)
		chosen := t.TempDir()
		defer setenv("HOME", chosen)()
		defer setenv("USERPROFILE", chosen)()

		code := RunWithTempHome(func() int {
			assert.Equal(t, chosen, userHomeDir(t))
			return 7
		})

		assert.Equal(t, 7, code)
		assert.Equal(t, chosen, userHomeDir(t))
		assert.Equal(t, suiteHome, os.Getenv(tempHomeEnv))
		assert.DirExists(t, suiteHome)
		return 0
	})
}

func TestHomeVariables(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{"HOME"}, homeVariables("linux", ""))
	assert.Equal(t, []string{"HOME"}, homeVariables("darwin", "/Users/me"))

	// Windows tests that set only USERPROFILE must not be shadowed by a HOME
	// the runner never had.
	assert.Equal(t, []string{"USERPROFILE"}, homeVariables("windows", ""))
	assert.Equal(t, []string{"USERPROFILE", "HOME"}, homeVariables("windows", `C:\Users\me`))
}

func TestSetenvRestoresUnsetVariable(t *testing.T) {
	const name = "TESTUTIL_UNSET_VARIABLE"
	t.Setenv(name, "") // Restores the variable after the test.
	require.NoError(t, os.Unsetenv(name))

	restore := setenv(name, "value")
	assert.Equal(t, "value", os.Getenv(name))

	restore()
	_, set := os.LookupEnv(name)
	assert.False(t, set)
}
