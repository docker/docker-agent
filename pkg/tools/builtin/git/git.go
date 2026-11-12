package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameGitStatus   = "git_status"
	ToolNameGitLog      = "git_log"
	ToolNameGitBranches = "git_branches"
	ToolNameGitShow     = "git_show"
	ToolNameGitBlame    = "git_blame"

	category        = "git"
	defaultLogLimit = 20
	maxLogLimit     = 200
	maxBlameLines   = 400
)

func CreateToolSet(runConfig *config.RuntimeConfig) (tools.ToolSet, error) {
	wd := runConfig.WorkingDir
	if wd == "" {
		var err error
		wd, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("determining working directory: %w", err)
		}
	}
	return New(wd), nil
}

type ToolSet struct {
	dir string
}

var (
	_ tools.ToolSet      = (*ToolSet)(nil)
	_ tools.Instructable = (*ToolSet)(nil)
)

func New(dir string) *ToolSet { return &ToolSet{dir: dir} }

func (t *ToolSet) open() (*gogit.Repository, error) {
	repo, err := gogit.PlainOpenWithOptions(t.dir, &gogit.PlainOpenOptions{
		DetectDotGit:          true,
		EnableDotGitCommonDir: true,
	})
	if err != nil {
		return nil, fmt.Errorf("opening git repository at %s: %w", t.dir, err)
	}
	return repo, nil
}

type StatusArgs struct{}

type LogArgs struct {
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum number of commits to return (default 20, capped at 200)"`
	Path  string `json:"path,omitempty" jsonschema:"Only show commits that touch this path"`
}

type BranchesArgs struct{}

type ShowArgs struct {
	Ref string `json:"ref,omitempty" jsonschema:"Commit hash or revision to show (default HEAD)"`
}

type BlameArgs struct {
	Path string `json:"path" jsonschema:"File path to blame, relative to the repository root"`
	Rev  string `json:"rev,omitempty" jsonschema:"Commit or revision to blame at (default HEAD)"`
}

func (t *ToolSet) status(_ context.Context, _ StatusArgs) (*tools.ToolCallResult, error) {
	repo, err := t.open()
	if err != nil {
		return tools.ResultError("Error: " + err.Error()), nil
	}
	wt, err := repo.Worktree()
	if err != nil {
		return tools.ResultError("Error: reading worktree: " + err.Error()), nil
	}
	st, err := wt.Status()
	if err != nil {
		return tools.ResultError("Error: reading status: " + err.Error()), nil
	}

	branch := "(no commits yet)"
	if head, err := repo.Head(); err == nil {
		if head.Name().IsBranch() {
			branch = head.Name().Short()
		} else {
			branch = "(detached HEAD)"
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "On branch %s\n", branch)

	paths := make([]string, 0, len(st))
	for p, fs := range st {
		if fs.Staging == gogit.Unmodified && fs.Worktree == gogit.Unmodified {
			continue
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		b.WriteString("Working tree clean.")
		return tools.ResultSuccess(b.String()), nil
	}
	sort.Strings(paths)
	fmt.Fprintf(&b, "%d changed file(s) [XY = staged/worktree; M=modified A=added D=deleted R=renamed ?=untracked]:\n", len(paths))
	for _, p := range paths {
		fs := st[p]
		fmt.Fprintf(&b, "  %c%c %s\n", fs.Staging, fs.Worktree, p)
	}
	return tools.ResultSuccess(b.String()), nil
}

func (t *ToolSet) log(_ context.Context, args LogArgs) (*tools.ToolCallResult, error) {
	repo, err := t.open()
	if err != nil {
		return tools.ResultError("Error: " + err.Error()), nil
	}

	opts := &gogit.LogOptions{Order: gogit.LogOrderCommitterTime}
	if args.Path != "" {
		p := args.Path
		opts.FileName = &p
	}
	iter, err := repo.Log(opts)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return tools.ResultSuccess("No commits yet."), nil
		}
		return tools.ResultError("Error: reading log: " + err.Error()), nil
	}
	defer iter.Close()

	limit := args.Limit
	if limit <= 0 {
		limit = defaultLogLimit
	}
	if limit > maxLogLimit {
		limit = maxLogLimit
	}

	var b strings.Builder
	count := 0
	err = iter.ForEach(func(c *object.Commit) error {
		if count >= limit {
			return storer.ErrStop
		}
		fmt.Fprintf(&b, "%s  %s  %s  %s\n",
			c.Hash.String()[:8], c.Author.When.Format("2006-01-02"), c.Author.Name, firstLine(c.Message))
		count++
		return nil
	})
	if err != nil {
		return tools.ResultError("Error: iterating log: " + err.Error()), nil
	}
	if count == 0 {
		return tools.ResultSuccess("No commits found."), nil
	}
	return tools.ResultSuccess(b.String()), nil
}

func (t *ToolSet) branches(_ context.Context, _ BranchesArgs) (*tools.ToolCallResult, error) {
	repo, err := t.open()
	if err != nil {
		return tools.ResultError("Error: " + err.Error()), nil
	}

	current := ""
	if head, err := repo.Head(); err == nil && head.Name().IsBranch() {
		current = head.Name().Short()
	}

	iter, err := repo.Branches()
	if err != nil {
		return tools.ResultError("Error: reading branches: " + err.Error()), nil
	}
	defer iter.Close()

	var names []string
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		names = append(names, ref.Name().Short())
		return nil
	})
	if err != nil {
		return tools.ResultError("Error: iterating branches: " + err.Error()), nil
	}
	if len(names) == 0 {
		return tools.ResultSuccess("No branches."), nil
	}
	sort.Strings(names)

	var b strings.Builder
	for _, n := range names {
		marker := "  "
		if n == current {
			marker = "* "
		}
		fmt.Fprintf(&b, "%s%s\n", marker, n)
	}
	return tools.ResultSuccess(b.String()), nil
}

func (t *ToolSet) show(_ context.Context, args ShowArgs) (*tools.ToolCallResult, error) {
	repo, err := t.open()
	if err != nil {
		return tools.ResultError("Error: " + err.Error()), nil
	}
	hash, err := resolveRev(repo, args.Ref)
	if err != nil {
		return tools.ResultError("Error: " + err.Error()), nil
	}
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return tools.ResultError("Error: reading commit: " + err.Error()), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "commit %s\n", commit.Hash.String())
	fmt.Fprintf(&b, "Author: %s <%s>\n", commit.Author.Name, commit.Author.Email)
	fmt.Fprintf(&b, "Date:   %s\n\n", commit.Author.When.Format("2006-01-02 15:04:05 -0700"))
	fmt.Fprintf(&b, "%s\n", strings.TrimRight(commit.Message, "\n"))

	if stats, err := commit.Stats(); err == nil && len(stats) > 0 {
		b.WriteString("\nChanged files:\n")
		for _, s := range stats {
			fmt.Fprintf(&b, "  %s (+%d, -%d)\n", s.Name, s.Addition, s.Deletion)
		}
	}
	return tools.ResultSuccess(b.String()), nil
}

func (t *ToolSet) blame(_ context.Context, args BlameArgs) (*tools.ToolCallResult, error) {
	if strings.TrimSpace(args.Path) == "" {
		return tools.ResultError("Error: path is required."), nil
	}
	repo, err := t.open()
	if err != nil {
		return tools.ResultError("Error: " + err.Error()), nil
	}
	hash, err := resolveRev(repo, args.Rev)
	if err != nil {
		return tools.ResultError("Error: " + err.Error()), nil
	}
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return tools.ResultError("Error: reading commit: " + err.Error()), nil
	}
	result, err := gogit.Blame(commit, args.Path)
	if err != nil {
		return tools.ResultError("Error: blaming " + args.Path + ": " + err.Error()), nil
	}

	var b strings.Builder
	for i, line := range result.Lines {
		if i >= maxBlameLines {
			fmt.Fprintf(&b, "... (%d more lines truncated)\n", len(result.Lines)-maxBlameLines)
			break
		}
		short := line.Hash.String()
		if len(short) > 8 {
			short = short[:8]
		}
		fmt.Fprintf(&b, "%s %-20s %4d| %s\n", short, line.Author, i+1, line.Text)
	}
	return tools.ResultSuccess(b.String()), nil
}

func resolveRev(repo *gogit.Repository, rev string) (plumbing.Hash, error) {
	if strings.TrimSpace(rev) == "" {
		rev = "HEAD"
	}
	h, err := repo.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("resolving revision %q: %w", rev, err)
	}
	return *h, nil
}

func firstLine(msg string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(msg), "\n")
	return first
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:                    ToolNameGitStatus,
			Category:                category,
			Description:             "Show the working tree status: current branch and changed files (staged, unstaged, untracked).",
			Parameters:              tools.MustSchemaFor[StatusArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.status),
			Annotations:             tools.ToolAnnotations{ReadOnlyHint: true, Title: "Git Status"},
			AddDescriptionParameter: true,
		},
		{
			Name:                    ToolNameGitLog,
			Category:                category,
			Description:             "Show recent commit history (hash, date, author, subject). Optionally limit the count or filter by path.",
			Parameters:              tools.MustSchemaFor[LogArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.log),
			Annotations:             tools.ToolAnnotations{ReadOnlyHint: true, Title: "Git Log"},
			AddDescriptionParameter: true,
		},
		{
			Name:                    ToolNameGitBranches,
			Category:                category,
			Description:             "List local branches, marking the current one with an asterisk.",
			Parameters:              tools.MustSchemaFor[BranchesArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.branches),
			Annotations:             tools.ToolAnnotations{ReadOnlyHint: true, Title: "Git Branches"},
			AddDescriptionParameter: true,
		},
		{
			Name:                    ToolNameGitShow,
			Category:                category,
			Description:             "Show a commit's metadata, message, and changed files with added/deleted line counts (default HEAD).",
			Parameters:              tools.MustSchemaFor[ShowArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.show),
			Annotations:             tools.ToolAnnotations{ReadOnlyHint: true, Title: "Git Show"},
			AddDescriptionParameter: true,
		},
		{
			Name:                    ToolNameGitBlame,
			Category:                category,
			Description:             "Show line-by-line authorship (commit and author) for a file at a revision (default HEAD).",
			Parameters:              tools.MustSchemaFor[BlameArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.blame),
			Annotations:             tools.ToolAnnotations{ReadOnlyHint: true, Title: "Git Blame"},
			AddDescriptionParameter: true,
		},
	}, nil
}

func (t *ToolSet) Instructions() string {
	return `## Git Tool

Read-only access to the working git repository:

- git_status — current branch and changed files.
- git_log — recent commits; supports limit and path filters.
- git_branches — local branches (current marked with *).
- git_show — a commit's metadata, message, and changed files.
- git_blame — line-by-line authorship for a file.

These tools never modify the repository. To stage, commit, or checkout, use the
shell tool.`
}
