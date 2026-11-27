package skills

import (
	"os"
	"os/exec"
	"path/filepath"
	stdruntime "runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSuiteCacheStaysInsideTempHome is the regression test for the review on
// #5018. TestLoadWithRemoteSources caches the remote skill under
// paths.GetCacheDir(), which follows %LocalAppData% on Windows and an absolute
// XDG_CACHE_HOME on Linux rather than HOME. Re-run it together with the
// isolation guard in a child process whose cache roots are preset outside the
// temporary home: the child suite must resolve its cache inside the temporary
// tree and leave the preset roots empty.
func TestSuiteCacheStaysInsideTempHome(t *testing.T) {
	if testing.Short() {
		t.Skip("re-runs the test binary")
	}

	self, err := os.Executable()
	require.NoError(t, err)

	preset := t.TempDir()
	cmd := exec.CommandContext(t.Context(), self,
		"-test.run=^(TestTempHomeIsolation|TestLoadWithRemoteSources)$",
		"-test.count=1",
		"-test.timeout=2m",
	)
	// The child must create its own temporary home instead of inheriting the
	// one this suite runs in; an empty marker value means "not set". The
	// presets stand in for a developer's real cache root.
	cmd.Env = append(os.Environ(),
		"TESTUTIL_TEMP_HOME=",
		"XDG_CACHE_HOME="+preset,
		"LocalAppData="+preset,
	)

	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "child suite failed:\n%s", output)

	entries, err := os.ReadDir(preset)
	require.NoError(t, err)
	assert.Empty(t, entries, "child suite wrote into the preset cache root %q", preset)
}

// TestSuiteCacheStaysInsideTempHomeWithRelativeTMPDIR is the regression test
// for the relative-TMPDIR review on #5018. Running the compiled test binary
// with TMPDIR=.tmp used to export a relative XDG_CACHE_HOME: os.UserCacheDir
// rejected it and paths.GetCacheDir fell back to <cwd>/tmp/.cagent-cache,
// outside the child's temporary home. The child suite then failed
// TestTempHomeIsolation and could leave cache residue under the relative
// TMPDIR that its cleanup does not own.
func TestSuiteCacheStaysInsideTempHomeWithRelativeTMPDIR(t *testing.T) {
	if testing.Short() {
		t.Skip("re-runs the test binary")
	}
	if stdruntime.GOOS == "windows" {
		t.Skip("os.TempDir ignores TMPDIR on Windows")
	}

	self, err := os.Executable()
	require.NoError(t, err)

	work := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(work, "tmp"), 0o700))

	cmd := exec.CommandContext(t.Context(), self,
		"-test.run=^(TestTempHomeIsolation|TestLoadWithRemoteSources)$",
		"-test.count=1",
		"-test.timeout=2m",
	)
	cmd.Dir = work
	// Clear inherited absolute cache roots so the relative TMPDIR is what
	// RunWithTempHome has to derive the cache root from.
	cmd.Env = append(os.Environ(),
		"TESTUTIL_TEMP_HOME=",
		"TMPDIR=tmp",
		"XDG_CACHE_HOME=",
		"LocalAppData=",
	)

	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "child suite failed with a relative TMPDIR:\n%s", output)

	entries, err := os.ReadDir(filepath.Join(work, "tmp"))
	require.NoError(t, err)
	assert.Empty(t, entries, "child suite left residue under the relative TMPDIR")
}
