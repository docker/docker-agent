package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
)

var fixedTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func sig() *object.Signature {
	return &object.Signature{Name: "Alice", Email: "alice@example.com", When: fixedTime}
}

func newRepo(t *testing.T) (string, *gogit.Repository) {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	return dir, repo
}

func addCommit(t *testing.T, repo *gogit.Repository, dir string, files map[string]string, msg string) plumbing.Hash {
	t.Helper()
	wt, err := repo.Worktree()
	require.NoError(t, err)
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
		_, err := wt.Add(name)
		require.NoError(t, err)
	}
	h, err := wt.Commit(msg, &gogit.CommitOptions{Author: sig()})
	require.NoError(t, err)
	return h
}

func TestGitUnbornHead(t *testing.T) {
	t.Parallel()
	dir, _ := newRepo(t)

	res, err := New(dir).status(t.Context(), StatusArgs{})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Output)
	require.Contains(t, res.Output, "no commits yet")

	logRes, err := New(dir).log(t.Context(), LogArgs{})
	require.NoError(t, err)
	require.False(t, logRes.IsError, logRes.Output)
	require.Contains(t, logRes.Output, "No commits yet")
}

func TestGitLogLimitIsCapped(t *testing.T) {
	t.Parallel()
	dir, repo := newRepo(t)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	for i := range maxLogLimit + 1 {
		_, err := wt.Commit("commit "+strconv.Itoa(i), &gogit.CommitOptions{Author: sig(), AllowEmptyCommits: true})
		require.NoError(t, err)
	}

	res, err := New(dir).log(t.Context(), LogArgs{Limit: 1000000})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Output)
	require.Len(t, strings.Split(strings.TrimRight(res.Output, "\n"), "\n"), maxLogLimit)
}

func TestGitLogPathFilter(t *testing.T) {
	t.Parallel()
	dir, repo := newRepo(t)
	addCommit(t, repo, dir, map[string]string{"a.txt": "1\n"}, "touch a")
	addCommit(t, repo, dir, map[string]string{"b.txt": "1\n"}, "touch b")

	res, err := New(dir).log(t.Context(), LogArgs{Path: "b.txt"})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Output)
	require.Contains(t, res.Output, "touch b")
	require.NotContains(t, res.Output, "touch a")
}

func TestGitShowExplicitAndInvalidRef(t *testing.T) {
	t.Parallel()
	dir, repo := newRepo(t)
	first := addCommit(t, repo, dir, map[string]string{"a.txt": "1\n"}, "first commit")
	addCommit(t, repo, dir, map[string]string{"a.txt": "2\n"}, "second commit")

	res, err := New(dir).show(t.Context(), ShowArgs{Ref: first.String()})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Output)
	require.Contains(t, res.Output, "first commit")
	require.NotContains(t, res.Output, "second commit")

	bad, err := New(dir).show(t.Context(), ShowArgs{Ref: "no-such-ref"})
	require.NoError(t, err)
	require.True(t, bad.IsError)
}

func TestGitBlameAtRev(t *testing.T) {
	t.Parallel()
	dir, repo := newRepo(t)
	first := addCommit(t, repo, dir, map[string]string{"a.txt": "original\n"}, "first")
	addCommit(t, repo, dir, map[string]string{"a.txt": "rewritten\n"}, "second")

	res, err := New(dir).blame(t.Context(), BlameArgs{Path: "a.txt", Rev: first.String()})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Output)
	require.Contains(t, res.Output, "original")
	require.NotContains(t, res.Output, "rewritten")
}

func TestGitBlameTruncates(t *testing.T) {
	t.Parallel()
	dir, repo := newRepo(t)
	var big strings.Builder
	for i := range maxBlameLines + 50 {
		big.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	addCommit(t, repo, dir, map[string]string{"big.txt": big.String()}, "add big")

	res, err := New(dir).blame(t.Context(), BlameArgs{Path: "big.txt"})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Output)
	require.Contains(t, res.Output, "more lines truncated")
}

func TestCreateToolSetUsesWorkingDir(t *testing.T) {
	t.Parallel()
	dir, repo := newRepo(t)
	addCommit(t, repo, dir, map[string]string{"a.txt": "x\n"}, "initial")

	ts, err := CreateToolSet(&config.RuntimeConfig{Config: config.Config{WorkingDir: dir}})
	require.NoError(t, err)
	toolz, err := ts.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, toolz, 5)
}

func TestGitStatus(t *testing.T) {
	t.Parallel()
	dir, repo := newRepo(t)
	addCommit(t, repo, dir, map[string]string{"a.txt": "hello\nworld\n"}, "initial")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new\n"), 0o644))

	res, err := New(dir).status(t.Context(), StatusArgs{})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Output)
	require.Contains(t, res.Output, "a.txt")
	require.Contains(t, res.Output, "b.txt")
}

func TestGitStatusClean(t *testing.T) {
	t.Parallel()
	dir, repo := newRepo(t)
	addCommit(t, repo, dir, map[string]string{"a.txt": "x\n"}, "initial")

	res, err := New(dir).status(t.Context(), StatusArgs{})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Contains(t, res.Output, "clean")
}

func TestGitLog(t *testing.T) {
	t.Parallel()
	dir, repo := newRepo(t)
	addCommit(t, repo, dir, map[string]string{"a.txt": "1\n"}, "first commit")
	addCommit(t, repo, dir, map[string]string{"a.txt": "2\n"}, "second commit")

	res, err := New(dir).log(t.Context(), LogArgs{})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Output)
	require.Contains(t, res.Output, "first commit")
	require.Contains(t, res.Output, "second commit")

	res2, err := New(dir).log(t.Context(), LogArgs{Limit: 1})
	require.NoError(t, err)
	require.Contains(t, res2.Output, "second commit")
	require.NotContains(t, res2.Output, "first commit")
}

func TestGitBranches(t *testing.T) {
	t.Parallel()
	dir, repo := newRepo(t)
	addCommit(t, repo, dir, map[string]string{"a.txt": "x\n"}, "initial")

	head, err := repo.Head()
	require.NoError(t, err)
	current := head.Name().Short()

	res, err := New(dir).branches(t.Context(), BranchesArgs{})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Output)
	require.Contains(t, res.Output, current)
	require.Contains(t, res.Output, "*")
}

func TestGitShow(t *testing.T) {
	t.Parallel()
	dir, repo := newRepo(t)
	addCommit(t, repo, dir, map[string]string{"a.txt": "one\ntwo\n"}, "add a.txt")

	res, err := New(dir).show(t.Context(), ShowArgs{})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Output)
	require.Contains(t, res.Output, "add a.txt")
	require.Contains(t, res.Output, "Alice")
	require.Contains(t, res.Output, "a.txt")
}

func TestGitBlame(t *testing.T) {
	t.Parallel()
	dir, repo := newRepo(t)
	addCommit(t, repo, dir, map[string]string{"a.txt": "line one\nline two\n"}, "initial")

	res, err := New(dir).blame(t.Context(), BlameArgs{Path: "a.txt"})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Output)
	require.Contains(t, res.Output, "alice@example.com")
	require.Contains(t, res.Output, "line one")
}

func TestGitBlameRequiresPath(t *testing.T) {
	t.Parallel()
	dir, repo := newRepo(t)
	addCommit(t, repo, dir, map[string]string{"a.txt": "x\n"}, "initial")

	res, err := New(dir).blame(t.Context(), BlameArgs{})
	require.NoError(t, err)
	require.True(t, res.IsError)
}

func TestGitLinkedWorktree(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	dir, repo := newRepo(t)
	addCommit(t, repo, dir, map[string]string{"a.txt": "parent content\n"}, "parent commit")
	parentHead, err := repo.Head()
	require.NoError(t, err)

	linkedDir := filepath.Join(t.TempDir(), "linked")
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	runGit("-C", dir, "worktree", "add", "-b", "linked", linkedDir)
	linkedRepo, err := gogit.PlainOpenWithOptions(linkedDir, &gogit.PlainOpenOptions{EnableDotGitCommonDir: true})
	require.NoError(t, err)
	linkedHead := addCommit(t, linkedRepo, linkedDir, map[string]string{"a.txt": "linked content\n"}, "linked commit")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "parent-only.txt"), []byte("parent\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(linkedDir, "linked-only.txt"), []byte("linked\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(linkedDir, "a.txt"), []byte("uncommitted\n"), 0o644))
	subdir := filepath.Join(linkedDir, "nested")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	for _, detached := range []bool{false, true} {
		branch := "linked"
		if detached {
			runGit("-C", linkedDir, "checkout", "--detach")
			branch = "(detached HEAD)"
		}
		t.Run(branch, func(t *testing.T) {
			for _, wd := range []string{linkedDir, subdir} {
				t.Run(filepath.Base(wd), func(t *testing.T) {
					ts, err := CreateToolSet(&config.RuntimeConfig{Config: config.Config{WorkingDir: wd}})
					require.NoError(t, err)
					tool := ts.(*ToolSet)

					status, err := tool.status(t.Context(), StatusArgs{})
					require.NoError(t, err)
					require.False(t, status.IsError, status.Output)
					require.Contains(t, status.Output, "On branch "+branch)
					require.Contains(t, status.Output, " M a.txt")
					require.Contains(t, status.Output, "linked-only.txt")
					require.NotContains(t, status.Output, "parent-only.txt")

					log, err := tool.log(t.Context(), LogArgs{})
					require.NoError(t, err)
					require.False(t, log.IsError, log.Output)
					require.Contains(t, log.Output, "linked commit")
					require.Contains(t, log.Output, "parent commit")

					branches, err := tool.branches(t.Context(), BranchesArgs{})
					require.NoError(t, err)
					require.False(t, branches.IsError, branches.Output)
					require.Contains(t, branches.Output, parentHead.Name().Short())
					require.Contains(t, branches.Output, "linked")
					if detached {
						require.NotContains(t, branches.Output, "*")
					} else {
						require.Contains(t, branches.Output, "* linked")
					}

					show, err := tool.show(t.Context(), ShowArgs{})
					require.NoError(t, err)
					require.False(t, show.IsError, show.Output)
					require.Contains(t, show.Output, linkedHead.String())
					require.Contains(t, show.Output, "linked commit")
					require.Contains(t, show.Output, "a.txt")

					blame, err := tool.blame(t.Context(), BlameArgs{Path: "a.txt"})
					require.NoError(t, err)
					require.False(t, blame.IsError, blame.Output)
					require.Contains(t, blame.Output, "linked content")
					require.NotContains(t, blame.Output, "parent content")
					require.NotContains(t, blame.Output, "uncommitted")
				})
			}
		})
	}
}

func TestGitNotARepo(t *testing.T) {
	t.Parallel()
	res, err := New(t.TempDir()).status(t.Context(), StatusArgs{})
	require.NoError(t, err)
	require.True(t, res.IsError)
}

func TestGitToolSetInterfaces(t *testing.T) {
	t.Parallel()
	ts := New(t.TempDir())
	toolz, err := ts.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, toolz, 5)
	require.NotEmpty(t, ts.Instructions())
}
