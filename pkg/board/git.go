package board

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/docker/docker-agent/pkg/atomicfile"
)

// removeWorktree removes a card's git worktree and its branch. Best-effort:
// the worktree may never have been created if the agent failed to start.
func removeWorktree(ctx context.Context, repoPath, worktreePath, branch string) {
	cmd := exec.CommandContext(ctx, "git", "worktree", "remove", "--force", worktreePath)
	cmd.Dir = repoPath
	_ = cmd.Run()

	cmd = exec.CommandContext(ctx, "git", "branch", "-D", branch)
	cmd.Dir = repoPath
	_ = cmd.Run()
}

// isGitRepo reports whether path is inside a git working tree. The output
// is checked, not just the exit code: inside a bare repo or a .git dir the
// command succeeds but prints "false".
func isGitRepo(ctx context.Context, path string) bool {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = path
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// worktreeDiff returns the full diff of all changes in the worktree relative
// to the merge-base with the upstream default branch. This includes
// committed, staged, unstaged, and untracked files. It never mutates the
// worktree.
func worktreeDiff(ctx context.Context, worktree string) (string, error) {
	// The worktree may not exist yet while the agent is still starting up
	// and docker-agent has not created it. Report no changes rather than an
	// error; any other stat failure (permissions, corrupt path) is surfaced.
	if _, err := os.Stat(worktree); os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}

	base, err := runGit(ctx, worktree, "merge-base", "HEAD", upstreamBase(ctx, worktree))
	if err != nil {
		return "", err
	}

	// Mark untracked files as intent-to-add so they appear in the diff — in
	// a throwaway copy of the index, so this read never mutates the
	// worktree's real index (which would surprise git status, stash, …).
	indexCopy, cleanup, err := copyIndex(ctx, worktree)
	if err != nil {
		return "", err
	}
	defer cleanup()

	env := append(os.Environ(), "GIT_INDEX_FILE="+indexCopy)
	if _, err := runGitEnv(ctx, worktree, env, "add", "--intent-to-add", "."); err != nil {
		return "", err
	}

	return runGitEnv(ctx, worktree, env, "diff", strings.TrimSpace(base))
}

// copyIndex copies the worktree's git index to a temporary file and returns
// its path and a cleanup func. A missing index (fresh repository) yields an
// empty temporary index.
func copyIndex(ctx context.Context, worktree string) (string, func(), error) {
	out, err := runGit(ctx, worktree, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return "", nil, err
	}
	indexPath := strings.TrimSpace(out)

	tmpDir, err := os.MkdirTemp("", "board-index-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	indexCopy := filepath.Join(tmpDir, "index")
	err = func() error {
		src, err := os.Open(indexPath)
		if os.IsNotExist(err) {
			// No index yet: start from an empty one.
			return atomicfile.Write(indexCopy, bytes.NewReader(nil), 0o600)
		}
		if err != nil {
			return err
		}
		defer func() { _ = src.Close() }()
		return atomicfile.Write(indexCopy, src, 0o600)
	}()
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("copy index: %w", err)
	}

	return indexCopy, cleanup, nil
}

// upstreamBase returns the ref worktrees branch from and diffs compare
// against: the default branch of the repository's upstream remote, as
// "<remote>/<branch>".
//
// Remote names are not universal: some users keep the canonical repo as
// "origin", others name it "upstream" and point "origin" at their fork. So
// the remote is detected rather than assumed: a remote named "upstream" wins
// when present, otherwise "origin". The branch is read from the remote's
// recorded HEAD; when that is not recorded, the conventional default
// branches are probed. Repositories without a usable remote fall back to
// the local default branch, and finally HEAD, so diffs keep working in
// local-only repositories.
func upstreamBase(ctx context.Context, dir string) string {
	remote := upstreamRemote(ctx, dir)
	if out, err := runGit(ctx, dir, "symbolic-ref", "--short", "refs/remotes/"+remote+"/HEAD"); err == nil {
		if ref := strings.TrimSpace(out); ref != "" {
			return ref
		}
	}
	for _, branch := range []string{"main", "master"} {
		ref := remote + "/" + branch
		if _, err := runGit(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/"+ref); err == nil {
			return ref
		}
	}
	for _, branch := range []string{"main", "master"} {
		if _, err := runGit(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
			return branch
		}
	}
	return "HEAD"
}

// upstreamRemote returns "upstream" when the repository has a remote by that
// name, otherwise "origin".
func upstreamRemote(ctx context.Context, dir string) string {
	out, err := runGit(ctx, dir, "remote")
	if err != nil {
		return "origin"
	}
	for remote := range strings.FieldsSeq(out) {
		if remote == "upstream" {
			return "upstream"
		}
	}
	return "origin"
}

// runGit runs `git <args...>` in dir and returns stdout, including stderr in
// any returned error.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	return runGitEnv(ctx, dir, nil, args...)
}

// runGitEnv is runGit with an explicit environment (nil inherits the process
// environment).
func runGitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", args[0], strings.TrimSpace(stderr.String()), err)
	}
	return string(out), nil
}
