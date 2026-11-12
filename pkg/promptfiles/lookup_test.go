package promptfiles

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const promptFile = "PROMPT.md"

func TestPathsInWorkDir(t *testing.T) {
	t.Parallel()

	workDir, homeDir := t.TempDir(), t.TempDir()
	path := writePrompt(t, workDir, "content")

	assert.Equal(t, []string{path}, Paths(workDir, homeDir, "", promptFile))
}

func TestPathsInParent(t *testing.T) {
	t.Parallel()

	workDir, homeDir := t.TempDir(), t.TempDir()
	path := writePrompt(t, workDir, "content")
	child := makeDir(t, workDir, "child")

	assert.Equal(t, []string{path}, Paths(child, homeDir, "", promptFile))
}

func TestPathsClosestMatchWins(t *testing.T) {
	t.Parallel()

	workDir, homeDir := t.TempDir(), t.TempDir()
	writePrompt(t, workDir, "parent")
	child := makeDir(t, workDir, "child")
	childPath := writePrompt(t, child, "child")

	assert.Equal(t, []string{childPath}, Paths(child, homeDir, "", promptFile))
}

func TestPathsNoMatch(t *testing.T) {
	t.Parallel()

	workDir, homeDir := t.TempDir(), t.TempDir()

	assert.Empty(t, Paths(workDir, homeDir, "", promptFile))
}

func TestPathsWorkDirAndHome(t *testing.T) {
	t.Parallel()

	workDir, homeDir := t.TempDir(), t.TempDir()
	workDirPath := writePrompt(t, workDir, "workdir content")
	homePath := writePrompt(t, homeDir, "home content")

	assert.Equal(t, []string{workDirPath, homePath}, Paths(workDir, homeDir, "", promptFile))
}

func TestPathsHomeOnly(t *testing.T) {
	t.Parallel()

	workDir, homeDir := t.TempDir(), t.TempDir()
	homePath := writePrompt(t, homeDir, "home content")

	assert.Equal(t, []string{homePath}, Paths(workDir, homeDir, "", promptFile))
}

func TestPathsDeduplicateHomeAndWorkDir(t *testing.T) {
	t.Parallel()

	// Same dir for workdir and home: the home match must not be appended
	// a second time.
	dir := t.TempDir()
	path := writePrompt(t, dir, "content")

	assert.Equal(t, []string{path}, Paths(dir, dir, "", promptFile))
}

func TestPathsWorkDirNestedInHome(t *testing.T) {
	t.Parallel()

	// Realistic case: workDir is a child of homeDir, and the prompt
	// file lives only at the home root. The hierarchy walk discovers
	// it; the home-dir lookup must not duplicate it.
	homeDir := t.TempDir()
	homePath := writePrompt(t, homeDir, "home content")
	workDir := makeDir(t, homeDir, "project")

	assert.Equal(t, []string{homePath}, Paths(workDir, homeDir, "", promptFile))
}

func TestPathsEmptyHomeDirSkipsHomeLookup(t *testing.T) {
	t.Parallel()

	// homeDir == "" disables the home lookup entirely, even if a
	// prompt file happens to exist at the user's real $HOME.
	workDir := t.TempDir()

	assert.Empty(t, Paths(workDir, "", "", promptFile))
}

func TestPathsKitOverridesHome(t *testing.T) {
	t.Parallel()

	// Inside a sandbox the host stages prompt files into the kit. The
	// real $HOME inside the sandbox is unrelated to the host's $HOME,
	// so when kitDir is set the kit takes the place of the home lookup
	// (homeDir is ignored).
	workDir, homeDir, kitDir := t.TempDir(), t.TempDir(), t.TempDir()
	writePrompt(t, homeDir, "sandbox $HOME content (must be ignored)")
	promptDir := makeDir(t, kitDir, KitSubdir)
	kitPath := writePrompt(t, promptDir, "kit content")

	assert.Equal(t, []string{kitPath}, Paths(workDir, homeDir, kitDir, promptFile))
}

func TestPathsWorkDirWinsOverKit(t *testing.T) {
	t.Parallel()

	// When the file lives inside the live workspace the cwd-walk finds
	// it; the kit copy is appended afterwards so callers see both —
	// matching the existing workdir+home semantics.
	workDir, homeDir, kitDir := t.TempDir(), t.TempDir(), t.TempDir()
	workPath := writePrompt(t, workDir, "workspace")
	promptDir := makeDir(t, kitDir, KitSubdir)
	kitPath := writePrompt(t, promptDir, "kit")

	assert.Equal(t, []string{workPath, kitPath}, Paths(workDir, homeDir, kitDir, promptFile))
}

// writePrompt creates promptFile with body inside dir and returns the
// absolute path.
func writePrompt(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, promptFile)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// makeDir creates a subdirectory named name inside parent and returns
// the absolute path.
func makeDir(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	require.NoError(t, os.Mkdir(dir, 0o755))
	return dir
}

func TestPathsDiscoveryFailureKeepsPartialMatches(t *testing.T) {
	t.Parallel()
	for _, location := range []string{"hierarchy", "home", "kit"} {
		t.Run(location, func(t *testing.T) {
			t.Parallel()
			parent, home, kit := t.TempDir(), t.TempDir(), ""
			valid := writePrompt(t, parent, "ancestor")
			work := makeDir(t, parent, "project")
			brokenDir := work
			switch location {
			case "home":
				brokenDir = home
			case "kit":
				kit = t.TempDir()
				brokenDir = makeDir(t, kit, KitSubdir)
			}
			broken := filepath.Join(brokenDir, promptFile)
			if err := os.Symlink(broken, broken); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			paths, err := PathsWithError(work, home, kit, promptFile)
			require.Error(t, err)
			assert.Equal(t, []string{valid}, paths)
			assert.Equal(t, paths, Paths(work, home, kit, promptFile))
			found, lookupErr := findInHierarchy(work, promptFile)
			assert.Equal(t, valid, found)
			if location == "hierarchy" {
				require.Error(t, lookupErr)
			} else {
				require.NoError(t, lookupErr)
			}
		})
	}
}

func TestPathsWithErrorMissingFiles(t *testing.T) {
	t.Parallel()
	paths, err := PathsWithError(t.TempDir(), t.TempDir(), "", "missing.md")
	require.NoError(t, err)
	assert.Empty(t, paths)
}
