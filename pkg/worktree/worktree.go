// Package worktree creates throwaway git worktrees so an agent can work in
// isolation from the user's checkout. The worktree shares the repository's
// object store but has its own working directory and branch, letting the
// user keep using the original checkout while the agent makes changes.
package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/worktree/namesgenerator"
)

// ErrNotGitRepository means the requested directory is not inside a git worktree.
var ErrNotGitRepository = errors.New("not a git repository")

// ErrInvalidName means the requested worktree name cannot be safely used as a
// directory and branch component.
var ErrInvalidName = errors.New("invalid worktree name")

// ErrInvalidBase means the --worktree-base ref could not be resolved or
// fetched.
var ErrInvalidBase = errors.New("invalid worktree base")

// ErrInvalidPRRef means a --worktree-pr value is not a PR number or URL.
var ErrInvalidPRRef = errors.New("invalid pull request reference")

// ErrGHNotFound means the GitHub CLI (gh) is required but not installed.
var ErrGHNotFound = errors.New("the GitHub CLI (gh) is required to check out a pull request")

// Worktree describes a git worktree created for an agent session.
type Worktree struct {
	// Dir is the absolute path of the worktree's working directory.
	Dir string
	// Branch is the branch checked out in the worktree.
	Branch string
	// Name is the worktree's name (the part after the "worktree-" branch prefix).
	Name string
	// SourceDir is the root of the repository the worktree was branched
	// from. The worktree lives under the data directory, far from the
	// original checkout, so setup hooks need this to copy untracked files
	// (.env, local config) git won't carry over.
	SourceDir string
	// BaseCommit is the commit the worktree's branch was created at. It is
	// used to detect commits added during the session (see [Status]).
	BaseCommit string
	// Warnings reports files that could not be copied into the worktree.
	Warnings []string
}

// Status describes whether a worktree holds work that would be lost if it
// were removed.
type Status struct {
	// Modified is true when tracked files have uncommitted changes.
	Modified bool
	// Untracked is true when the worktree contains untracked files.
	Untracked bool
	// NewCommits is true when commits were added since the worktree was
	// created (HEAD moved away from [Worktree.BaseCommit]).
	NewCommits bool
}

// IsDirty reports whether the worktree holds any work (uncommitted changes,
// untracked files, or new commits) that removing it would discard.
func (s Status) IsDirty() bool {
	return s.Modified || s.Untracked || s.NewCommits
}

// CreateOption customizes how [Create] and [CreatePR] build a worktree.
type CreateOption func(*createConfig)

type createConfig struct {
	base string
	root string
}

// resolveCreateConfig applies opts on top of the defaults. The worktrees root
// defaults to the data directory; [WithRoot] overrides it (mainly for tests
// and embedders that need an isolated location).
func resolveCreateConfig(opts []CreateOption) createConfig {
	cfg := createConfig{root: paths.GetDataDir()}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// WithBase branches the worktree from ref instead of the repository's current
// HEAD. ref is any revision git understands (a branch, tag, commit, or
// remote-tracking ref like "origin/main"). A remote-tracking ref is fetched
// first so the worktree starts from the latest remote state. An empty ref is
// ignored, keeping the default HEAD behaviour.
func WithBase(ref string) CreateOption {
	return func(c *createConfig) { c.base = strings.TrimSpace(ref) }
}

// WithRoot places the worktree under dir instead of the default data
// directory. The worktree is created at <dir>/worktrees/<name>. An empty dir
// is ignored, keeping the default. This decouples the package from the
// process-global data directory, letting tests and embedders pick an isolated
// location.
func WithRoot(dir string) CreateOption {
	return func(c *createConfig) {
		if dir != "" {
			c.root = dir
		}
	}
}

// Create creates a new git worktree for the repository containing dir and
// returns it. The worktree lives under the data directory and checks out a
// freshly created branch so the agent's changes stay isolated from the user's
// checkout.
//
// When name is empty, a friendly random name (e.g. "focused_turing") is
// generated. The branch is named "worktree-<name>" and the worktree is stored
// under <root>/worktrees/<name>, where root defaults to the data directory and
// can be overridden with [WithRoot].
//
// By default the branch starts at the repository's current HEAD. Pass
// [WithBase] to branch from another ref instead; when the base is a
// remote-tracking ref (e.g. "origin/main") it is fetched first so the worktree
// starts from the latest remote state.
//
// Returns [ErrNotGitRepository] when dir is not inside a git worktree, and
// [ErrInvalidName] when an explicit name is not a safe path/branch component.
func Create(ctx context.Context, dir, name string, opts ...CreateOption) (*Worktree, error) {
	cfg := resolveCreateConfig(opts)

	root, err := repoRoot(ctx, dir)
	if err != nil {
		return nil, err
	}

	if name == "" {
		name = namesgenerator.GetRandomName(0)
	} else if err := validateName(name); err != nil {
		return nil, err
	}

	branch := "worktree-" + name
	dest := filepath.Join(cfg.root, "worktrees", name)

	if _, err := os.Stat(dest); err == nil {
		return nil, fmt.Errorf("%w: worktree %q already exists at %s", ErrInvalidName, name, dest)
	}

	// The branch start-point: a user-chosen base ref, or the current HEAD.
	// `git worktree add -b <branch> <dest> [<start-point>]` omits the
	// start-point when none was requested, preserving the default behaviour.
	addArgs := []string{"worktree", "add", "-b", branch, dest}
	if cfg.base != "" {
		if err := fetchBase(ctx, root, cfg.base); err != nil {
			return nil, err
		}
		addArgs = append(addArgs, cfg.base)
	}

	if err := git(ctx, root, addArgs...); err != nil {
		if cfg.base != "" {
			return nil, fmt.Errorf("%w: %s: %w", ErrInvalidBase, cfg.base, err)
		}
		return nil, fmt.Errorf("creating git worktree: %w", err)
	}

	wt := &Worktree{Dir: dest, Branch: branch, Name: name, SourceDir: root}

	// Record the branch point so [Status] can later tell whether the
	// session added commits. A brand-new repository with no commits has no
	// HEAD yet; leave BaseCommit empty in that case.
	if head, err := gitOutput(ctx, dest, "rev-parse", "HEAD"); err == nil {
		wt.BaseCommit = head
	}

	wt.Warnings = copyIncludedFiles(ctx, root, dir, dest)
	return wt, nil
}

// CreatePR creates a git worktree that checks out an existing GitHub pull
// request so the agent can continue it. ref is a PR number ("123") or a
// GitHub pull request URL. The PR's head branch is checked out tracking its
// remote, so commits made in the worktree push back to the pull request.
//
// It delegates PR resolution to the GitHub CLI (gh), which handles head-branch
// lookup, fork remotes, and upstream tracking. Returns [ErrGHNotFound] when gh
// is not installed, [ErrInvalidPRRef] when ref is malformed, and
// [ErrNotGitRepository] when dir is not inside a git repository.
func CreatePR(ctx context.Context, dir, ref string, opts ...CreateOption) (*Worktree, error) {
	cfg := resolveCreateConfig(opts)

	number, err := parsePRRef(ref)
	if err != nil {
		return nil, err
	}

	root, err := repoRoot(ctx, dir)
	if err != nil {
		return nil, err
	}

	if _, err := exec.LookPath("gh"); err != nil {
		return nil, ErrGHNotFound
	}

	name := fmt.Sprintf("pr-%d", number)
	dest := filepath.Join(cfg.root, "worktrees", name)
	if _, err := os.Stat(dest); err == nil {
		return nil, fmt.Errorf("%w: worktree %q already exists at %s", ErrInvalidName, name, dest)
	}

	// Create the worktree first (detached at HEAD), then let gh check out the
	// PR head into it. gh resolves the PR's head branch, adds a fork remote
	// when needed, and sets upstream tracking so pushes return to the PR.
	if err := git(ctx, root, "worktree", "add", "--detach", dest); err != nil {
		return nil, fmt.Errorf("creating git worktree: %w", err)
	}

	if err := gh(ctx, dest, "pr", "checkout", strconv.Itoa(number)); err != nil {
		// Roll back the empty worktree so a failed checkout leaves no trace.
		_ = git(ctx, root, "worktree", "remove", "--force", dest)
		return nil, fmt.Errorf("checking out pull request #%d: %w", number, err)
	}

	branch, err := gitOutput(ctx, dest, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolving pull request branch: %w", err)
	}

	wt := &Worktree{Dir: dest, Branch: branch, Name: name, SourceDir: root}
	if head, err := gitOutput(ctx, dest, "rev-parse", "HEAD"); err == nil {
		wt.BaseCommit = head
	}
	wt.Warnings = copyIncludedFiles(ctx, root, dir, dest)
	return wt, nil
}

// parsePRRef extracts a PR number from a bare number ("123", "#123") or a
// GitHub pull request URL (".../pull/123").
func parsePRRef(ref string) (int, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, fmt.Errorf("%w: empty reference", ErrInvalidPRRef)
	}

	candidate := strings.TrimPrefix(ref, "#")
	if strings.Contains(ref, "://") || strings.Contains(ref, "/pull/") {
		// Pull the segment after "/pull/" from a URL like
		// https://github.com/owner/repo/pull/123(/files|#discussion...).
		_, rest, ok := strings.Cut(ref, "/pull/")
		if !ok {
			return 0, fmt.Errorf("%w: %q", ErrInvalidPRRef, ref)
		}
		candidate, _, _ = strings.Cut(rest, "/")
		candidate, _, _ = strings.Cut(candidate, "#")
		candidate, _, _ = strings.Cut(candidate, "?")
	}

	number, err := strconv.Atoi(candidate)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("%w: %q", ErrInvalidPRRef, ref)
	}
	return number, nil
}

// Status inspects the worktree and reports whether it holds uncommitted
// changes, untracked files, or commits added since creation.
func (wt *Worktree) Status(ctx context.Context) (Status, error) {
	out, err := gitOutput(ctx, wt.Dir, "status", "--porcelain")
	if err != nil {
		return Status{}, fmt.Errorf("inspecting worktree: %w", err)
	}

	var st Status
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "??") {
			st.Untracked = true
		} else {
			st.Modified = true
		}
	}

	// HEAD moving away from the recorded branch point means the session
	// committed something. When BaseCommit is empty (worktree created in a
	// repo without commits) any resolvable HEAD counts as new work.
	if head, err := gitOutput(ctx, wt.Dir, "rev-parse", "HEAD"); err == nil {
		st.NewCommits = head != wt.BaseCommit
	}

	return st, nil
}

// Remove deletes the worktree's directory and its branch, discarding any
// uncommitted changes, untracked files, and commits. Callers decide when
// removal is appropriate (e.g. only for worktrees they created, never
// pre-existing ones).
func (wt *Worktree) Remove(ctx context.Context) error {
	if err := git(ctx, wt.SourceDir, "worktree", "remove", "--force", wt.Dir); err != nil {
		return fmt.Errorf("removing git worktree: %w", err)
	}
	// The branch can only be deleted once the worktree no longer occupies it;
	// -D discards unmerged commits, which is the intended "remove and forget".
	if err := git(ctx, wt.SourceDir, "branch", "-D", wt.Branch); err != nil {
		return fmt.Errorf("deleting worktree branch: %w", err)
	}
	return nil
}

// validateName rejects names that would escape the worktrees directory or
// produce an invalid git branch. Names must be a single path segment made of
// safe characters, which also keeps the derived "worktree-<name>" branch valid.
func validateName(name string) error {
	if name != strings.TrimSpace(name) {
		return fmt.Errorf("%w: %q has surrounding whitespace", ErrInvalidName, name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("%w: %q must not contain path separators", ErrInvalidName, name)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%w: %q is not allowed", ErrInvalidName, name)
	}
	// filepath.Base collapses separators and ".."; if the cleaned segment
	// differs, the input was not a plain single path component.
	if filepath.Base(name) != name {
		return fmt.Errorf("%w: %q must be a single path segment", ErrInvalidName, name)
	}
	return nil
}

// repoRoot returns the worktree root of the git repository containing dir,
// or [ErrNotGitRepository] when dir is not inside one.
func repoRoot(ctx context.Context, dir string) (string, error) {
	out, err := gitOutput(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", ErrNotGitRepository
		}
		return "", err
	}
	return filepath.Clean(out), nil
}

// fetchBase updates the local copy of a remote-tracking base ref (e.g.
// "origin/main") so the worktree branches from the latest remote state. Refs
// that don't name a remote (a local branch, tag, or commit) are left alone.
// A fetch failure is reported as [ErrInvalidBase].
//
// A fully-qualified refspec (refs/heads/<branch>:refs/remotes/<remote>/<branch>)
// is used so the remote-tracking ref is updated regardless of the repository's
// fetch configuration (custom refspecs, --no-tags, tagOpt, ...). A bare
// "git fetch <remote> <branch>" only guarantees FETCH_HEAD is written, which
// would leave the subsequent "git worktree add ... <remote>/<branch>"
// resolving a stale remote-tracking ref.
func fetchBase(ctx context.Context, root, base string) error {
	remote, branch, ok := strings.Cut(base, "/")
	if !ok {
		return nil
	}
	if !isRemote(ctx, root, remote) {
		return nil
	}
	refspec := "refs/heads/" + branch + ":refs/remotes/" + remote + "/" + branch
	if err := git(ctx, root, "fetch", remote, refspec); err != nil {
		return fmt.Errorf("%w: fetching %s: %w", ErrInvalidBase, base, err)
	}
	return nil
}

// isRemote reports whether name is a configured git remote.
func isRemote(ctx context.Context, root, name string) bool {
	out, err := gitOutput(ctx, root, "remote")
	if err != nil {
		return false
	}
	for remote := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(remote) == name {
			return true
		}
	}
	return false
}

func git(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// gitOutput runs a git command in dir and returns its trimmed stdout.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

// gh runs a GitHub CLI command in dir, surfacing its stderr on failure.
func gh(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}
