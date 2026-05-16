package builtins_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// initGitRepo creates a temp directory, runs `git init` in it, and
// returns the path. The repo is configured with a deterministic
// committer identity and `commit.gpgsign=false` so subsequent test
// commits work in any environment (including CI sandboxes that don't
// have a default user.email or that inherit a global signing config).
//
// Skips the test (rather than failing it) when the `git` binary is
// missing. The git-shelling builtins are designed to no-op gracefully
// when git isn't installed, but their behavior tests want a real repo
// to assert on; skipping keeps the suite green on minimal images.
func initGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed; skipping git-backed builtin test")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet", "-b", "main")
	runGit(t, dir, "config", "user.email", "tester@example.com")
	runGit(t, dir, "config", "user.name", "Tester")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	return dir
}

// runGit executes `git -C dir args...` and fails the test on any
// non-zero exit, attaching the captured combined output to the failure
// message so a missing user.email or similar setup issue is obvious in
// the test log.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(t.Context(), "git", full...)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v failed: %s", args, out)
}

// writeFile is a thin shorthand for creating a file inside dir with the
// given content; it's only here so that arrange-blocks read like prose
// rather than a litany of os/filepath calls.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
}
