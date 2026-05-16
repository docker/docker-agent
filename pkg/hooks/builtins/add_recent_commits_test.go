package builtins_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
)

// TestAddRecentCommitsListsHistory verifies the happy path: in a repo
// with three commits, the default invocation emits all three subjects
// in newest-first order. We assert on subject substrings rather than
// the SHA prefix because hashes are non-deterministic.
func TestAddRecentCommitsListsHistory(t *testing.T) {
	t.Parallel()

	dir := initGitRepo(t)
	for _, subject := range []string{"first commit", "second commit", "third commit"} {
		writeFile(t, dir, "log.txt", subject)
		runGit(t, dir, "add", "log.txt")
		runGit(t, dir, "commit", "--quiet", "-m", subject)
	}

	fn := lookup(t, builtins.AddRecentCommits)

	out, err := fn(t.Context(), &hooks.Input{SessionID: "s", Cwd: dir}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput)
	assert.Equal(t, hooks.EventSessionStart, out.HookSpecificOutput.HookEventName,
		"add_recent_commits must target session_start, not turn_start")

	ctx := out.HookSpecificOutput.AdditionalContext
	for _, subject := range []string{"first commit", "second commit", "third commit"} {
		assert.Contains(t, ctx, subject)
	}
	// `git log` emits newest-first, so "third" must precede "first".
	assert.Less(t, strings.Index(ctx, "third commit"), strings.Index(ctx, "first commit"),
		"recent commits must be listed newest-first")
}

// TestAddRecentCommitsHonorsLimitArg pins the args contract: a numeric
// first arg overrides the default 10-commit limit. With three commits
// in the repo and limit=1, only the most recent subject must appear.
func TestAddRecentCommitsHonorsLimitArg(t *testing.T) {
	t.Parallel()

	dir := initGitRepo(t)
	for _, subject := range []string{"alpha", "beta", "gamma"} {
		writeFile(t, dir, "log.txt", subject)
		runGit(t, dir, "add", "log.txt")
		runGit(t, dir, "commit", "--quiet", "-m", subject)
	}

	fn := lookup(t, builtins.AddRecentCommits)

	out, err := fn(t.Context(), &hooks.Input{SessionID: "s", Cwd: dir}, []string{"1"})
	require.NoError(t, err)
	require.NotNil(t, out)

	ctx := out.HookSpecificOutput.AdditionalContext
	assert.Contains(t, ctx, "gamma", "newest commit must be present")
	assert.NotContains(t, ctx, "beta", "limit=1 must drop the older commits")
	assert.NotContains(t, ctx, "alpha", "limit=1 must drop the older commits")
}

// TestAddRecentCommitsInvalidLimitFallsBack documents the lenient-args
// contract: a non-numeric or non-positive limit is logged at debug
// and the builtin falls back to the default. This avoids the awkward
// failure mode where a typo in YAML disables the hook.
func TestAddRecentCommitsInvalidLimitFallsBack(t *testing.T) {
	t.Parallel()

	dir := initGitRepo(t)
	writeFile(t, dir, "log.txt", "only commit")
	runGit(t, dir, "add", "log.txt")
	runGit(t, dir, "commit", "--quiet", "-m", "only commit")

	fn := lookup(t, builtins.AddRecentCommits)

	for _, arg := range []string{"abc", "0", "-5"} {
		out, err := fn(t.Context(), &hooks.Input{SessionID: "s", Cwd: dir}, []string{arg})
		require.NoError(t, err)
		require.NotNilf(t, out, "invalid arg %q must fall back to default rather than no-op", arg)
		assert.Contains(t, out.HookSpecificOutput.AdditionalContext, "only commit",
			"fallback must still emit the existing history")
	}
}

// TestAddRecentCommitsEmptyRepoIsNoop documents that a freshly-initted
// repo with no commits yet produces nil rather than an empty "Recent
// commits:" stanza. `git log` exits non-zero in that case, which the
// builtin treats as graceful no-op.
func TestAddRecentCommitsEmptyRepoIsNoop(t *testing.T) {
	t.Parallel()

	dir := initGitRepo(t)

	fn := lookup(t, builtins.AddRecentCommits)

	out, err := fn(t.Context(), &hooks.Input{SessionID: "s", Cwd: dir}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

// TestAddRecentCommitsNoCwdOrNotARepoIsNoop documents the safety /
// graceful failure paths: empty Cwd, nil input, and a non-repo
// directory all produce nil rather than aborting session start.
func TestAddRecentCommitsNoCwdOrNotARepoIsNoop(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.AddRecentCommits)

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
