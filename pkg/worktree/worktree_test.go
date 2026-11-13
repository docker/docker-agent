package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateWorktree(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	root := t.TempDir()

	wt, err := Create(t.Context(), dir, "", WithRoot(root))
	require.NoError(t, err)
	require.NotNil(t, wt)

	assert.DirExists(t, wt.Dir)
	// A random name looks like "focused_turing" (adjective_surname).
	assert.Regexp(t, `^[a-z]+_[a-z]+$`, wt.Name)
	assert.Equal(t, "worktree-"+wt.Name, wt.Branch)
	assert.Equal(t, filepath.Join(root, "worktrees", wt.Name), wt.Dir)

	// The worktree shares the repository's history: the initial commit's
	// files must be present in the new working directory.
	assert.FileExists(t, filepath.Join(wt.Dir, "a.txt"))

	// The checked-out branch must match the one reported by the worktree.
	out := gitOut(t, wt.Dir, "rev-parse", "--abbrev-ref", "HEAD")
	assert.Equal(t, wt.Branch, out)
}

func TestCreateWithName(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	root := t.TempDir()

	wt, err := Create(t.Context(), dir, "my-feature", WithRoot(root))
	require.NoError(t, err)

	assert.Equal(t, "my-feature", wt.Name)
	assert.Equal(t, "worktree-my-feature", wt.Branch)
	assert.Equal(t, filepath.Join(root, "worktrees", "my-feature"), wt.Dir)
	assert.FileExists(t, filepath.Join(wt.Dir, "a.txt"))
}

func TestCreateWithBaseBranchesFromRef(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)

	// Add a second commit on a side branch; the default HEAD (main) stays at
	// the first commit.
	runGit(t, dir, "branch", "feature")
	runGit(t, dir, "checkout", "feature")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("B"), 0o644))
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "feature work")
	featureHead := gitOut(t, dir, "rev-parse", "feature")
	runGit(t, dir, "checkout", "-")

	wt, err := Create(t.Context(), dir, "from-feature", WithRoot(t.TempDir()), WithBase("feature"))
	require.NoError(t, err)

	// The worktree branched from feature, so it carries feature's commit and
	// its file.
	assert.Equal(t, featureHead, gitOut(t, wt.Dir, "rev-parse", "HEAD"))
	assert.FileExists(t, filepath.Join(wt.Dir, "b.txt"))
	assert.Equal(t, featureHead, wt.BaseCommit)
}

func TestCreateWithEmptyBaseUsesHead(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)

	head := gitOut(t, dir, "rev-parse", "HEAD")

	// An empty base is ignored: the worktree branches from the current HEAD.
	wt, err := Create(t.Context(), dir, "default-base", WithRoot(t.TempDir()), WithBase(""))
	require.NoError(t, err)
	assert.Equal(t, head, gitOut(t, wt.Dir, "rev-parse", "HEAD"))
}

func TestCreateWithInvalidBase(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)

	_, err := Create(t.Context(), dir, "bad-base", WithRoot(t.TempDir()), WithBase("does-not-exist"))
	assert.ErrorIs(t, err, ErrInvalidBase)
}

func TestCreateWithRemoteBaseFetches(t *testing.T) {
	t.Parallel()
	// A bare "remote" repo with one extra commit beyond what the clone has.
	remote := bootstrapRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(remote, "remote-only.txt"), []byte("R"), 0o644))
	runGit(t, remote, "add", ".")
	runGit(t, remote, "commit", "-m", "remote work")
	remoteBranch := gitOut(t, remote, "rev-parse", "--abbrev-ref", "HEAD")

	// A clone that is behind: it lacks the remote's latest commit until
	// WithBase triggers a fetch.
	clone, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	runGit(t, clone, "clone", remote, ".")
	runGit(t, clone, "config", "user.email", "test@example.com")
	runGit(t, clone, "config", "user.name", "Test User")
	runGit(t, clone, "config", "commit.gpgsign", "false")
	// Move the remote forward again so the clone's remote-tracking ref is
	// stale; only an explicit fetch (done by WithBase) brings it up to date.
	require.NoError(t, os.WriteFile(filepath.Join(remote, "newer.txt"), []byte("N"), 0o644))
	runGit(t, remote, "add", ".")
	runGit(t, remote, "commit", "-m", "newer work")
	newerHead := gitOut(t, remote, "rev-parse", "HEAD")

	wt, err := Create(t.Context(), clone, "from-remote", WithRoot(t.TempDir()), WithBase("origin/"+remoteBranch))
	require.NoError(t, err)

	// The fetch pulled the latest remote commit, so the worktree starts there.
	assert.Equal(t, newerHead, gitOut(t, wt.Dir, "rev-parse", "HEAD"))
	assert.FileExists(t, filepath.Join(wt.Dir, "newer.txt"))
}

func TestCreateWithRemoteBaseUpdatesTrackingRefWithoutFetchConfig(t *testing.T) {
	t.Parallel()
	// A remote with one extra commit beyond what the clone has.
	remote := bootstrapRepo(t)
	remoteBranch := gitOut(t, remote, "rev-parse", "--abbrev-ref", "HEAD")

	clone, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	runGit(t, clone, "clone", remote, ".")
	runGit(t, clone, "config", "user.email", "test@example.com")
	runGit(t, clone, "config", "user.name", "Test User")
	runGit(t, clone, "config", "commit.gpgsign", "false")
	// Drop the default fetch refspec so a bare "git fetch origin <branch>"
	// would only write FETCH_HEAD and leave refs/remotes/origin/<branch>
	// stale. fetchBase must use an explicit refspec to update it anyway.
	runGit(t, clone, "config", "--unset-all", "remote.origin.fetch")

	require.NoError(t, os.WriteFile(filepath.Join(remote, "newer.txt"), []byte("N"), 0o644))
	runGit(t, remote, "add", ".")
	runGit(t, remote, "commit", "-m", "newer work")
	newerHead := gitOut(t, remote, "rev-parse", "HEAD")

	wt, err := Create(t.Context(), clone, "from-remote-noconfig", WithRoot(t.TempDir()), WithBase("origin/"+remoteBranch))
	require.NoError(t, err)

	// The explicit refspec updated the remote-tracking ref despite the missing
	// fetch config, so the worktree starts at the latest remote commit.
	assert.Equal(t, newerHead, gitOut(t, wt.Dir, "rev-parse", "HEAD"))
	assert.FileExists(t, filepath.Join(wt.Dir, "newer.txt"))
}

func TestCreateFromSubfolder(t *testing.T) {
	t.Parallel()
	repoRoot := bootstrapRepo(t)
	sub := filepath.Join(repoRoot, "nested")
	require.NoError(t, os.MkdirAll(sub, 0o755))

	wt, err := Create(t.Context(), sub, "", WithRoot(t.TempDir()))
	require.NoError(t, err)
	assert.DirExists(t, wt.Dir)
	assert.FileExists(t, filepath.Join(wt.Dir, "a.txt"))
}

func TestCreateOutsideGitRepo(t *testing.T) {
	t.Parallel()
	_, err := Create(t.Context(), t.TempDir(), "", WithRoot(t.TempDir()))
	assert.ErrorIs(t, err, ErrNotGitRepository)
}

func TestCreateRejectsUnsafeNames(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	root := t.TempDir()

	for _, name := range []string{
		"../escape",
		"../../etc/evil",
		"foo/bar",
		`foo\bar`,
		".",
		"..",
		" leading",
		"trailing ",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Create(t.Context(), dir, name, WithRoot(root))
			assert.ErrorIs(t, err, ErrInvalidName)
		})
	}
}

func TestCreateRejectsDuplicateName(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	root := t.TempDir()

	_, err := Create(t.Context(), dir, "dup", WithRoot(root))
	require.NoError(t, err)

	_, err = Create(t.Context(), dir, "dup", WithRoot(root))
	assert.ErrorIs(t, err, ErrInvalidName)
}

func TestStatusClean(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)

	wt, err := Create(t.Context(), dir, "clean", WithRoot(t.TempDir()))
	require.NoError(t, err)

	st, err := wt.Status(t.Context())
	require.NoError(t, err)
	assert.False(t, st.IsDirty())
	assert.False(t, st.Modified)
	assert.False(t, st.Untracked)
	assert.False(t, st.NewCommits)
}

func TestStatusDetectsUntracked(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)

	wt, err := Create(t.Context(), dir, "untracked", WithRoot(t.TempDir()))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(wt.Dir, "new.txt"), []byte("x"), 0o644))

	st, err := wt.Status(t.Context())
	require.NoError(t, err)
	assert.True(t, st.IsDirty())
	assert.True(t, st.Untracked)
	assert.False(t, st.Modified)
	assert.False(t, st.NewCommits)
}

func TestStatusDetectsModified(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)

	wt, err := Create(t.Context(), dir, "modified", WithRoot(t.TempDir()))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(wt.Dir, "a.txt"), []byte("changed"), 0o644))

	st, err := wt.Status(t.Context())
	require.NoError(t, err)
	assert.True(t, st.IsDirty())
	assert.True(t, st.Modified)
	assert.False(t, st.Untracked)
	assert.False(t, st.NewCommits)
}

func TestStatusDetectsNewCommits(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)

	wt, err := Create(t.Context(), dir, "committed", WithRoot(t.TempDir()))
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(wt.Dir, "a.txt"), []byte("changed"), 0o644))
	runGit(t, wt.Dir, "commit", "-am", "work")

	st, err := wt.Status(t.Context())
	require.NoError(t, err)
	assert.True(t, st.IsDirty())
	assert.True(t, st.NewCommits)
	// A committed change leaves a clean tree.
	assert.False(t, st.Modified)
	assert.False(t, st.Untracked)
}

func TestRemove(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)

	wt, err := Create(t.Context(), dir, "gone", WithRoot(t.TempDir()))
	require.NoError(t, err)
	require.DirExists(t, wt.Dir)

	require.NoError(t, wt.Remove(t.Context()))
	assert.NoDirExists(t, wt.Dir)

	// The branch must be gone too.
	branches := gitOut(t, dir, "branch", "--list", wt.Branch)
	assert.Empty(t, branches)
}

func TestRemoveDiscardsDirtyWork(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)

	wt, err := Create(t.Context(), dir, "dirty", WithRoot(t.TempDir()))
	require.NoError(t, err)
	// Uncommitted change + untracked file + a new commit: removal must
	// still succeed and wipe everything.
	require.NoError(t, os.WriteFile(filepath.Join(wt.Dir, "a.txt"), []byte("changed"), 0o644))
	runGit(t, wt.Dir, "commit", "-am", "work")
	require.NoError(t, os.WriteFile(filepath.Join(wt.Dir, "untracked.txt"), []byte("x"), 0o644))

	require.NoError(t, wt.Remove(t.Context()))
	assert.NoDirExists(t, wt.Dir)
}

func bootstrapRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test User")
	// Keep the test hermetic: never sign commits with the developer's
	// global signing setup (e.g. a 1Password SSH agent), which is
	// unavailable/flaky in test environments and breaks `git commit`.
	runGit(t, dir, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A"), 0o644))
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	require.NoError(t, err)
	return string(trimNL(out))
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func TestParsePRRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{in: "123", want: 123},
		{in: "#123", want: 123},
		{in: "  42 ", want: 42},
		{in: "https://github.com/owner/repo/pull/123", want: 123},
		{in: "https://github.com/owner/repo/pull/123/files", want: 123},
		{in: "https://github.com/owner/repo/pull/123#issuecomment-1", want: 123},
		{in: "https://github.com/owner/repo/pull/7?w=1", want: 7},
		{in: "", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "0", wantErr: true},
		{in: "-3", wantErr: true},
		{in: "https://github.com/owner/repo/issues/123", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := parsePRRef(tt.in)
			if tt.wantErr {
				assert.ErrorIs(t, err, ErrInvalidPRRef)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestCreatePRValidatesRefBeforeGit checks a malformed ref fails fast with
// ErrInvalidPRRef, before touching git or gh, even outside a repository.
func TestCreatePRValidatesRefBeforeGit(t *testing.T) {
	t.Parallel()
	_, err := CreatePR(t.Context(), t.TempDir(), "not-a-pr")
	assert.ErrorIs(t, err, ErrInvalidPRRef)
}

// TestCreateWaitsForInflightAdd pins the per-repository serialization of
// `git worktree add` (#4922): concurrent adds in one repo race on the
// .git/worktrees admin entries.
func TestCreateWaitsForInflightAdd(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)

	root, err := repoRoot(t.Context(), dir)
	require.NoError(t, err)
	unlock := lockAdds(root)

	worktreeRoot := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := Create(t.Context(), dir, "blocked", WithRoot(worktreeRoot))
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("Create finished while another add held the repo lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	unlock()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Create did not finish after the repo lock was released")
	}
}
