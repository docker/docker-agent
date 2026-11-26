package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	stdruntime "runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
)

// relativeTMPDIRChildEnv marks the helper process of
// TestRunWithTempHomeWithRelativeTMPDIR.
const relativeTMPDIRChildEnv = "TESTUTIL_RELATIVE_TMPDIR_CHILD"

// TestRunWithTempHomeWithRelativeTMPDIR is the regression test for the
// relative-TMPDIR review on #5018. os.MkdirTemp builds the temporary home from
// TMPDIR as supplied, so a relative TMPDIR used to make RunWithTempHome export
// a relative XDG_CACHE_HOME. os.UserCacheDir rejects a relative cache root and
// paths.GetCacheDir falls back to <relative TMPDIR>/.cagent-cache, outside the
// suite home; RequireTempHome then fails and cache-writing tests leave residue
// that the suite cleanup does not remove.
//
// The child process is needed because this suite may already run under
// RunWithTempHome, whose marker would make a nested call inherit the parent
// home instead of creating one from TMPDIR.
func TestRunWithTempHomeWithRelativeTMPDIR(t *testing.T) {
	if testing.Short() {
		t.Skip("re-executes the test binary")
	}
	if stdruntime.GOOS == "windows" {
		t.Skip("os.TempDir ignores TMPDIR on Windows")
	}

	self, err := os.Executable()
	require.NoError(t, err)

	work := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(work, "tmp"), 0o700))

	cmd := exec.CommandContext(t.Context(), self, "-test.run=^TestRunWithTempHomeRelativeTMPDIRChild$", "-test.count=1", "-test.timeout=1m")
	cmd.Dir = work
	// The child must create its own temporary home instead of inheriting
	// this suite's, and create it through a relative TMPDIR.
	cmd.Env = append(os.Environ(),
		"TESTUTIL_TEMP_HOME=",
		"TMPDIR=tmp",
		relativeTMPDIRChildEnv+"=1",
	)

	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "child with a relative TMPDIR failed:\n%s", output)

	// The temporary home lived under tmp/ and was removed on exit, so the
	// relative TMPDIR must not have left the fallback cache behind.
	entries, err := os.ReadDir(filepath.Join(work, "tmp"))
	require.NoError(t, err)
	assert.Empty(t, entries, "child left residue under the relative TMPDIR")
}

// TestRunWithTempHomeRelativeTMPDIRChild is the helper process of
// TestRunWithTempHomeWithRelativeTMPDIR, run with TMPDIR=tmp.
func TestRunWithTempHomeRelativeTMPDIRChild(t *testing.T) {
	if os.Getenv(relativeTMPDIRChildEnv) != "1" {
		t.Skip("helper process for TestRunWithTempHomeWithRelativeTMPDIR")
	}

	RunWithTempHome(func() int {
		home := os.Getenv(tempHomeEnv)
		if !filepath.IsAbs(home) {
			t.Errorf("temporary home %q is relative: a relative TMPDIR must not leak into the exported environment", home)
		}
		cache := paths.GetCacheDir()
		if !filepath.IsAbs(cache) {
			t.Errorf("cache directory %q is relative", cache)
		}
		if !withinDir(home, cache) {
			t.Errorf("cache directory %q is outside the temporary home %q", cache, home)
		}
		return 0
	})
}
