package builtins_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
)

// TestAddGitDiffStatModeShowsTouchedFile verifies the default mode
// (`git diff --stat`): after staging an initial commit and modifying a
// tracked file, the diff output must mention the file. We anchor the
// assertion on the filename rather than the stat formatting so the
// test is stable across git versions.
func TestAddGitDiffStatModeShowsTouchedFile(t *testing.T) {
	t.Parallel()

	dir := initGitRepo(t)
	writeFile(t, dir, "README.md", "first")
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "--quiet", "-m", "init")
	// Modify the tracked file so `git diff` (working-tree vs HEAD)
	// has something to report.
	writeFile(t, dir, "README.md", "second")

	fn := lookup(t, builtins.AddGitDiff)

	out, err := fn(t.Context(), &hooks.Input{SessionID: "s", Cwd: dir}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput)
	assert.Equal(t, hooks.EventTurnStart, out.HookSpecificOutput.HookEventName,
		"add_git_diff must target turn_start, not session_start")
	assert.Contains(t, out.HookSpecificOutput.AdditionalContext, "README.md")
	assert.Contains(t, out.HookSpecificOutput.AdditionalContext, "stat",
		"default mode must announce itself as the stat view")
}

// TestAddGitDiffFullModeShowsHunks pins the args contract: passing the
// single arg "full" switches from `--stat` to a full unified diff. We
// assert on the presence of a "+second" line, which only appears in
// the unified output, not in --stat.
func TestAddGitDiffFullModeShowsHunks(t *testing.T) {
	t.Parallel()

	dir := initGitRepo(t)
	writeFile(t, dir, "README.md", "first\n")
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "--quiet", "-m", "init")
	writeFile(t, dir, "README.md", "second\n")

	fn := lookup(t, builtins.AddGitDiff)

	out, err := fn(t.Context(), &hooks.Input{SessionID: "s", Cwd: dir}, []string{"full"})
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Contains(t, out.HookSpecificOutput.AdditionalContext, "+second",
		"full mode must include the unified-diff hunk lines")
}

// TestAddGitDiffCleanTreeIsNoop documents that a clean working tree
// produces a nil Output rather than an empty stanza. The model should
// not see "Current working-tree diff (stat):\n" with nothing under it.
func TestAddGitDiffCleanTreeIsNoop(t *testing.T) {
	t.Parallel()

	dir := initGitRepo(t)
	writeFile(t, dir, "README.md", "first")
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "--quiet", "-m", "init")

	fn := lookup(t, builtins.AddGitDiff)

	out, err := fn(t.Context(), &hooks.Input{SessionID: "s", Cwd: dir}, nil)
	require.NoError(t, err)
	assert.Nil(t, out, "clean tree must yield no additional context")
}

// TestAddGitDiffNoCwdOrNotARepoIsNoop documents the safety / graceful
// failure paths: empty Cwd, nil input, and a non-repo directory all
// produce nil rather than aborting the session.
func TestAddGitDiffNoCwdOrNotARepoIsNoop(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.AddGitDiff)

	out, err := fn(t.Context(), &hooks.Input{SessionID: "s"}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)

	out, err = fn(t.Context(), nil, nil)
	require.NoError(t, err)
	assert.Nil(t, out)

	out, err = fn(t.Context(), &hooks.Input{SessionID: "s", Cwd: t.TempDir()}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}
