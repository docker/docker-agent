package builtins

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const promptFile = "PROMPT.md"

func TestPromptFilePathsInWorkDir(t *testing.T) {
	t.Parallel()

	workDir, homeDir := t.TempDir(), t.TempDir()
	path := writePrompt(t, workDir, "content")

	assert.Equal(t, []string{path}, promptFilePaths(workDir, homeDir, promptFile))
}

func TestPromptFilePathsInParent(t *testing.T) {
	t.Parallel()

	workDir, homeDir := t.TempDir(), t.TempDir()
	path := writePrompt(t, workDir, "content")
	child := makeDir(t, workDir, "child")

	assert.Equal(t, []string{path}, promptFilePaths(child, homeDir, promptFile))
}

func TestPromptFilePathsClosestMatchWins(t *testing.T) {
	t.Parallel()

	workDir, homeDir := t.TempDir(), t.TempDir()
	writePrompt(t, workDir, "parent")
	child := makeDir(t, workDir, "child")
	childPath := writePrompt(t, child, "child")

	assert.Equal(t, []string{childPath}, promptFilePaths(child, homeDir, promptFile))
}

func TestPromptFilePathsNoMatch(t *testing.T) {
	t.Parallel()

	workDir, homeDir := t.TempDir(), t.TempDir()

	assert.Empty(t, promptFilePaths(workDir, homeDir, promptFile))
}

func TestPromptFilePathsWorkDirAndHome(t *testing.T) {
	t.Parallel()

	workDir, homeDir := t.TempDir(), t.TempDir()
	workDirPath := writePrompt(t, workDir, "workdir content")
	homePath := writePrompt(t, homeDir, "home content")

	assert.Equal(t, []string{workDirPath, homePath}, promptFilePaths(workDir, homeDir, promptFile))
}

func TestPromptFilePathsHomeOnly(t *testing.T) {
	t.Parallel()

	workDir, homeDir := t.TempDir(), t.TempDir()
	homePath := writePrompt(t, homeDir, "home content")

	assert.Equal(t, []string{homePath}, promptFilePaths(workDir, homeDir, promptFile))
}

func TestPromptFilePathsDeduplicateHomeAndWorkDir(t *testing.T) {
	t.Parallel()

	// Same dir for workdir and home: the home match must not be appended
	// a second time.
	dir := t.TempDir()
	path := writePrompt(t, dir, "content")

	assert.Equal(t, []string{path}, promptFilePaths(dir, dir, promptFile))
}

func TestPromptFilePathsWorkDirNestedInHome(t *testing.T) {
	t.Parallel()

	// Realistic case: workDir is a child of homeDir, and the prompt
	// file lives only at the home root. The hierarchy walk discovers
	// it; the home-dir lookup must not duplicate it.
	homeDir := t.TempDir()
	homePath := writePrompt(t, homeDir, "home content")
	workDir := makeDir(t, homeDir, "project")

	assert.Equal(t, []string{homePath}, promptFilePaths(workDir, homeDir, promptFile))
}

func TestPromptFilePathsEmptyHomeDirSkipsHomeLookup(t *testing.T) {
	t.Parallel()

	// homeDir == "" disables the home lookup entirely, even if a
	// prompt file happens to exist at the user's real $HOME.
	workDir := t.TempDir()

	assert.Empty(t, promptFilePaths(workDir, "", promptFile))
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
