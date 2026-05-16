// Package snapshot records workspace states in a shadow git repository.
package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/paths"
)

const (
	pruneAfter     = "7.days"
	largeFileLimit = 2 * 1024 * 1024
)

// ErrNotGitRepository means the requested directory is not inside a git worktree.
var ErrNotGitRepository = errors.New("not a git repository")

// Patch describes files changed since a snapshot tree hash.
type Patch struct {
	Hash  string   `json:"hash"`
	Files []string `json:"files"`
}

// FileDiff describes one file in a diff between two snapshot tree hashes.
type FileDiff struct {
	File      string `json:"file"`
	Patch     string `json:"patch"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Status    string `json:"status,omitempty"`
}

type revertOp struct {
	hash string
	file string
	rel  string
}

// Manager opens per-worktree shadow repositories under a data directory.
type Manager struct {
	dataDir string
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
}

// NewManager creates a snapshot manager rooted at dataDir.
func NewManager(dataDir string) *Manager {
	if dataDir == "" {
		dataDir = paths.GetDataDir()
	}
	return &Manager{dataDir: dataDir, locks: map[string]*sync.Mutex{}}
}

// Open returns the shadow repository for the git worktree containing dir.
func (m *Manager) Open(ctx context.Context, dir string) (*Repo, error) {
	if dir == "" {
		return nil, ErrNotGitRepository
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	worktree, err := gitWorktree(ctx, abs)
	if err != nil {
		return nil, err
	}
	gitdir := filepath.Join(m.dataDir, "snapshot", hashPath(worktree))
	return &Repo{
		directory: abs,
		worktree:  worktree,
		gitdir:    gitdir,
		lock:      m.lock(gitdir),
	}, nil
}

// Cleanup runs garbage collection for the shadow repository containing dir.
func (m *Manager) Cleanup(ctx context.Context, dir string) error {
	repo, err := m.Open(ctx, dir)
	if err != nil {
		return err
	}
	return repo.Cleanup(ctx)
}

func (m *Manager) lock(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.locks[key]; l != nil {
		return l
	}
	l := &sync.Mutex{}
	m.locks[key] = l
	return l
}

// Repo is a shadow git repository paired with a source worktree.
type Repo struct {
	directory string
	worktree  string
	gitdir    string
	lock      *sync.Mutex
}

// Directory returns the directory used to scope pathspecs for this repo.
func (r *Repo) Directory() string { return r.directory }

// Worktree returns the source git worktree root.
func (r *Repo) Worktree() string { return r.worktree }

// GitDir returns the shadow git directory.
func (r *Repo) GitDir() string { return r.gitdir }

// Track stages the current source worktree state and returns its tree hash.
func (r *Repo) Track(ctx context.Context) (string, error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	if err := r.ensure(ctx); err != nil {
		return "", err
	}
	if err := r.add(ctx); err != nil {
		return "", err
	}
	result := r.git(ctx, r.args("write-tree"), gitOpts{cwd: r.directory})
	if result.err != nil {
		return "", result.err
	}
	if result.code != 0 {
		return "", fmt.Errorf("git write-tree failed: %s", strings.TrimSpace(result.stderr))
	}
	hash := strings.TrimSpace(result.stdout)
	slog.DebugContext(ctx, "snapshot tracking", "hash", hash, "cwd", r.directory, "gitdir", r.gitdir)
	return hash, nil
}

// Patch reports files that changed between hash and the current source state.
func (r *Repo) Patch(ctx context.Context, hash string) (Patch, error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	if err := r.ensure(ctx); err != nil {
		return Patch{Hash: hash}, err
	}
	if err := r.add(ctx); err != nil {
		return Patch{Hash: hash}, err
	}
	result := r.git(ctx, append(quoteArgs(), r.args("diff", "--cached", "--no-ext-diff", "--name-only", hash, "--", ".")...), gitOpts{cwd: r.directory})
	if result.err != nil {
		return Patch{Hash: hash}, result.err
	}
	if result.code != 0 {
		slog.WarnContext(ctx, "failed to get snapshot diff", "hash", hash, "exit_code", result.code, "stderr", result.stderr)
		return Patch{Hash: hash, Files: []string{}}, nil
	}
	files := lines(result.stdout)
	ignored := r.ignore(ctx, files)
	out := make([]string, 0, len(files))
	for _, file := range files {
		if ignored[file] {
			continue
		}
		out = append(out, filepath.ToSlash(filepath.Join(r.worktree, filepath.FromSlash(file))))
	}
	return Patch{Hash: hash, Files: out}, nil
}

// Diff returns a unified diff between hash and the current source state.
func (r *Repo) Diff(ctx context.Context, hash string) (string, error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	if err := r.ensure(ctx); err != nil {
		return "", err
	}
	if err := r.add(ctx); err != nil {
		return "", err
	}
	result := r.git(ctx, append(quoteArgs(), r.args("diff", "--cached", "--no-ext-diff", hash, "--", ".")...), gitOpts{cwd: r.directory})
	if result.err != nil {
		return "", result.err
	}
	if result.code != 0 {
		slog.WarnContext(ctx, "failed to get snapshot diff", "hash", hash, "exit_code", result.code, "stderr", result.stderr)
		return "", nil
	}
	return strings.TrimSpace(result.stdout), nil
}

// Restore checks out all files from hash into the source worktree.
func (r *Repo) Restore(ctx context.Context, hash string) error {
	r.lock.Lock()
	defer r.lock.Unlock()
	if err := r.ensure(ctx); err != nil {
		return err
	}
	result := r.git(ctx, append(coreArgs(), r.args("read-tree", hash)...), gitOpts{cwd: r.worktree})
	if result.err != nil {
		return result.err
	}
	if result.code != 0 {
		return fmt.Errorf("restore snapshot %s: git read-tree failed: %s", hash, strings.TrimSpace(result.stderr))
	}
	checkout := r.git(ctx, append(coreArgs(), r.args("checkout-index", "-a", "-f")...), gitOpts{cwd: r.worktree})
	if checkout.err != nil {
		return checkout.err
	}
	if checkout.code != 0 {
		return fmt.Errorf("restore snapshot %s: git checkout-index failed: %s", hash, strings.TrimSpace(checkout.stderr))
	}
	return nil
}

// Revert restores or removes the files listed in patches.
func (r *Repo) Revert(ctx context.Context, patches []Patch) error {
	r.lock.Lock()
	defer r.lock.Unlock()
	if err := r.ensure(ctx); err != nil {
		return err
	}
	ops := make([]revertOp, 0)
	seen := map[string]bool{}
	for _, patch := range patches {
		for _, file := range patch.Files {
			abs, err := filepath.Abs(file)
			if err != nil {
				continue
			}
			if seen[abs] {
				continue
			}
			rel, err := filepath.Rel(r.worktree, abs)
			if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
				continue
			}
			seen[abs] = true
			ops = append(ops, revertOp{hash: patch.Hash, file: abs, rel: filepath.ToSlash(rel)})
		}
	}

	single := func(op revertOp) error {
		slog.DebugContext(ctx, "reverting snapshot file", "file", op.file, "hash", op.hash)
		checkout := r.git(ctx, append(coreArgs(), r.args("checkout", op.hash, "--", op.rel)...), gitOpts{cwd: r.worktree})
		if checkout.err != nil {
			return checkout.err
		}
		if checkout.code == 0 {
			return nil
		}
		tree := r.git(ctx, append(coreArgs(), r.args("ls-tree", op.hash, "--", op.rel)...), gitOpts{cwd: r.worktree})
		if tree.err != nil {
			return tree.err
		}
		if tree.code == 0 && strings.TrimSpace(tree.stdout) != "" {
			slog.DebugContext(ctx, "snapshot file existed but checkout failed; keeping", "file", op.file, "hash", op.hash)
			return nil
		}
		slog.DebugContext(ctx, "snapshot file did not exist; deleting", "file", op.file, "hash", op.hash)
		return removeFile(op.file)
	}

	for i := 0; i < len(ops); {
		first := ops[i]
		run := []revertOp{first}
		j := i + 1
		for j < len(ops) && len(run) < 100 {
			next := ops[j]
			if next.hash != first.hash || anyClash(run, next.rel) {
				break
			}
			run = append(run, next)
			j++
		}
		if len(run) == 1 {
			if err := single(first); err != nil {
				return err
			}
			i = j
			continue
		}

		rels := make([]string, 0, len(run))
		for _, op := range run {
			rels = append(rels, op.rel)
		}
		tree := r.git(ctx, append(coreArgs(), r.args(append([]string{"ls-tree", "--name-only", first.hash, "--"}, rels...)...)...), gitOpts{cwd: r.worktree})
		if tree.err != nil {
			return tree.err
		}
		if tree.code != 0 {
			for _, op := range run {
				if err := single(op); err != nil {
					return err
				}
			}
			i = j
			continue
		}
		have := map[string]bool{}
		for _, item := range lines(tree.stdout) {
			have[item] = true
		}
		var checkoutRels []string
		for _, op := range run {
			if have[op.rel] {
				checkoutRels = append(checkoutRels, op.rel)
			}
		}
		if len(checkoutRels) > 0 {
			checkout := r.git(ctx, append(coreArgs(), r.args(append([]string{"checkout", first.hash, "--"}, checkoutRels...)...)...), gitOpts{cwd: r.worktree})
			if checkout.err != nil {
				return checkout.err
			}
			if checkout.code != 0 {
				for _, op := range run {
					if err := single(op); err != nil {
						return err
					}
				}
				i = j
				continue
			}
		}
		for _, op := range run {
			if have[op.rel] {
				continue
			}
			if err := removeFile(op.file); err != nil {
				return err
			}
		}
		i = j
	}
	return nil
}

// Cleanup runs git gc for this shadow repository.
func (r *Repo) Cleanup(ctx context.Context) error {
	r.lock.Lock()
	defer r.lock.Unlock()
	if _, err := os.Stat(r.gitdir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	result := r.git(ctx, r.args("gc", "--prune="+pruneAfter), gitOpts{cwd: r.directory})
	if result.err != nil {
		return result.err
	}
	if result.code != 0 {
		return fmt.Errorf("snapshot cleanup failed: %s", strings.TrimSpace(result.stderr))
	}
	return nil
}

// DiffFull returns file-level diff metadata between two snapshot tree hashes.
func (r *Repo) DiffFull(ctx context.Context, from, to string) ([]FileDiff, error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	if err := r.ensure(ctx); err != nil {
		return nil, err
	}
	statuses := r.git(ctx, append(quoteArgs(), r.args("diff", "--no-ext-diff", "--name-status", "--no-renames", from, to, "--", ".")...), gitOpts{cwd: r.directory})
	if statuses.err != nil {
		return nil, statuses.err
	}
	if statuses.code != 0 {
		return nil, fmt.Errorf("snapshot diff status failed: %s", strings.TrimSpace(statuses.stderr))
	}
	statusByFile := map[string]string{}
	for _, line := range lines(statuses.stdout) {
		code, file, ok := strings.Cut(line, "\t")
		if !ok || file == "" {
			continue
		}
		switch {
		case strings.HasPrefix(code, "A"):
			statusByFile[file] = "added"
		case strings.HasPrefix(code, "D"):
			statusByFile[file] = "deleted"
		default:
			statusByFile[file] = "modified"
		}
	}

	numstat := r.git(ctx, append(quoteArgs(), r.args("diff", "--no-ext-diff", "--no-renames", "--numstat", from, to, "--", ".")...), gitOpts{cwd: r.directory})
	if numstat.err != nil {
		return nil, numstat.err
	}
	if numstat.code != 0 {
		return nil, fmt.Errorf("snapshot diff numstat failed: %s", strings.TrimSpace(numstat.stderr))
	}
	type row struct {
		file      string
		binary    bool
		additions int
		deletions int
	}
	rows := make([]row, 0)
	for _, line := range lines(numstat.stdout) {
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		rows = append(rows, row{
			file:      parts[2],
			binary:    parts[0] == "-" && parts[1] == "-",
			additions: parseNumstat(parts[0]),
			deletions: parseNumstat(parts[1]),
		})
	}
	ignored := r.ignore(ctx, func() []string {
		files := make([]string, 0, len(rows))
		for _, row := range rows {
			files = append(files, row.file)
		}
		return files
	}())
	out := make([]FileDiff, 0, len(rows))
	for _, row := range rows {
		if ignored[row.file] {
			continue
		}
		patch := ""
		if !row.binary {
			p := r.git(ctx, append(quoteArgs(), r.args("diff", "--no-ext-diff", "--no-renames", from, to, "--", row.file)...), gitOpts{cwd: r.directory})
			if p.err != nil {
				return nil, p.err
			}
			if p.code == 0 {
				patch = p.stdout
			}
		}
		out = append(out, FileDiff{
			File:      row.file,
			Patch:     patch,
			Additions: row.additions,
			Deletions: row.deletions,
			Status:    statusByFile[row.file],
		})
	}
	return out, nil
}

func (r *Repo) ensure(ctx context.Context) error {
	if err := os.MkdirAll(r.gitdir, 0o755); err != nil { //nolint:gosec // 0o755 matches the layout `git init` itself creates
		return err
	}
	if _, err := os.Stat(filepath.Join(r.gitdir, "HEAD")); err == nil {
		return nil
	}
	result := r.git(ctx, []string{"init"}, gitOpts{env: map[string]string{
		"GIT_DIR":       r.gitdir,
		"GIT_WORK_TREE": r.worktree,
	}})
	if result.err != nil {
		return result.err
	}
	if result.code != 0 {
		return fmt.Errorf("snapshot git init failed: %s", strings.TrimSpace(result.stderr))
	}
	for _, args := range [][]string{
		{"config", "core.autocrlf", "false"},
		{"config", "core.longpaths", "true"},
		{"config", "core.symlinks", "true"},
		{"config", "core.fsmonitor", "false"},
	} {
		cfg := r.git(ctx, append([]string{"--git-dir", r.gitdir}, args...), gitOpts{})
		if cfg.err != nil {
			return cfg.err
		}
		if cfg.code != 0 {
			return fmt.Errorf("snapshot git config failed: %s", strings.TrimSpace(cfg.stderr))
		}
	}
	slog.DebugContext(ctx, "initialized snapshot repository", "gitdir", r.gitdir, "worktree", r.worktree)
	return nil
}

func (r *Repo) add(ctx context.Context) error {
	if err := r.syncExcludes(ctx, nil); err != nil {
		return err
	}
	diff := r.git(ctx, append(quoteArgs(), r.args("diff-files", "--name-only", "-z", "--", ".")...), gitOpts{cwd: r.directory})
	other := r.git(ctx, append(quoteArgs(), r.args("ls-files", "--others", "--exclude-standard", "-z", "--", ".")...), gitOpts{cwd: r.directory})
	if diff.err != nil {
		return diff.err
	}
	if other.err != nil {
		return other.err
	}
	if diff.code != 0 || other.code != 0 {
		return fmt.Errorf("list snapshot files failed: diff=%d %s other=%d %s", diff.code, strings.TrimSpace(diff.stderr), other.code, strings.TrimSpace(other.stderr))
	}
	tracked := splitNUL(diff.stdout)
	untracked := splitNUL(other.stdout)
	all := unique(append(tracked, untracked...))
	if len(all) == 0 {
		return nil
	}
	ignored := r.ignore(ctx, all)
	if len(ignored) > 0 {
		ignoredFiles := keys(ignored)
		slog.DebugContext(ctx, "removing gitignored files from snapshot", "count", len(ignoredFiles))
		if err := r.drop(ctx, ignoredFiles); err != nil {
			return err
		}
	}
	allow := make([]string, 0, len(all))
	for _, item := range all {
		if !ignored[item] {
			allow = append(allow, item)
		}
	}
	if len(allow) == 0 {
		return nil
	}
	large := map[string]bool{}
	for _, item := range allow {
		info, err := os.Lstat(filepath.Join(r.worktree, filepath.FromSlash(item)))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.Size() > largeFileLimit {
			large[item] = true
		}
	}
	block := make([]string, 0)
	for _, item := range untracked {
		if large[item] {
			block = append(block, item)
		}
	}
	if err := r.syncExcludes(ctx, block); err != nil {
		return err
	}
	stage := make([]string, 0, len(allow))
	for _, item := range allow {
		if !slices.Contains(block, item) {
			stage = append(stage, item)
		}
	}
	return r.stage(ctx, stage)
}

func (r *Repo) ignore(ctx context.Context, files []string) map[string]bool {
	out := map[string]bool{}
	if len(files) == 0 {
		return out
	}
	result := command(ctx, []string{"-C", r.worktree, "-c", "core.quotepath=false", "check-ignore", "--no-index", "--stdin", "-z"}, gitOpts{cwd: r.directory, stdin: []byte(strings.Join(files, "\x00") + "\x00")})
	if result.err != nil || (result.code != 0 && result.code != 1) {
		return out
	}
	for _, item := range splitNUL(result.stdout) {
		out[item] = true
	}
	return out
}

func (r *Repo) drop(ctx context.Context, files []string) error {
	if len(files) == 0 {
		return nil
	}
	result := r.git(ctx, append(cfgArgs(), r.args("rm", "--cached", "-f", "--ignore-unmatch", "--pathspec-from-file=-", "--pathspec-file-nul")...), gitOpts{cwd: r.directory, stdin: nulList(files)})
	if result.err != nil {
		return result.err
	}
	if result.code != 0 {
		return fmt.Errorf("snapshot git rm failed: %s", strings.TrimSpace(result.stderr))
	}
	return nil
}

func (r *Repo) stage(ctx context.Context, files []string) error {
	if len(files) == 0 {
		return nil
	}
	result := r.git(ctx, append(cfgArgs(), r.args("add", "--all", "--sparse", "--pathspec-from-file=-", "--pathspec-file-nul")...), gitOpts{cwd: r.directory, stdin: nulList(files)})
	if result.err != nil {
		return result.err
	}
	if result.code != 0 {
		slog.WarnContext(ctx, "failed to add snapshot files", "exit_code", result.code, "stderr", result.stderr)
	}
	return nil
}

func (r *Repo) syncExcludes(ctx context.Context, list []string) error {
	source := ""
	result := command(ctx, []string{"-C", r.worktree, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude"}, gitOpts{})
	if result.err == nil && result.code == 0 {
		file := strings.TrimSpace(result.stdout)
		if file != "" {
			if data, err := os.ReadFile(file); err == nil {
				source = strings.TrimRight(string(data), "\r\n")
			}
		}
	}
	parts := make([]string, 0, 1+len(list))
	if source != "" {
		parts = append(parts, source)
	}
	for _, item := range list {
		parts = append(parts, "/"+filepath.ToSlash(item))
	}
	text := strings.Join(parts, "\n")
	if text != "" {
		text += "\n"
	}
	target := filepath.Join(r.gitdir, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // mirrors what git writes into .git/info
		return err
	}
	return os.WriteFile(target, []byte(text), 0o644) //nolint:gosec // mirrors what git writes for .git/info/exclude
}

func (r *Repo) args(cmd ...string) []string {
	out := make([]string, 0, 4+len(cmd))
	out = append(out, "--git-dir", r.gitdir, "--work-tree", r.worktree)
	out = append(out, cmd...)
	return out
}

func (r *Repo) git(ctx context.Context, args []string, opts gitOpts) gitResult {
	return command(ctx, args, opts)
}

type gitOpts struct {
	cwd   string
	env   map[string]string
	stdin []byte
}

type gitResult struct {
	code   int
	stdout string
	stderr string
	err    error
}

func command(ctx context.Context, args []string, opts gitOpts) gitResult {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = opts.cwd
	if len(opts.env) > 0 {
		cmd.Env = os.Environ()
		for key, value := range opts.env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	if opts.stdin != nil {
		cmd.Stdin = bytes.NewReader(opts.stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := gitResult{stdout: stdout.String(), stderr: stderr.String()}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.code = exitErr.ExitCode()
			return res
		}
		res.code = -1
		res.err = err
		return res
	}
	return res
}

func gitWorktree(ctx context.Context, dir string) (string, error) {
	inside := command(ctx, []string{"-C", dir, "rev-parse", "--is-inside-work-tree"}, gitOpts{})
	if inside.err != nil {
		return "", inside.err
	}
	if inside.code != 0 || strings.TrimSpace(inside.stdout) != "true" {
		return "", ErrNotGitRepository
	}
	root := command(ctx, []string{"-C", dir, "rev-parse", "--show-toplevel"}, gitOpts{})
	if root.err != nil {
		return "", root.err
	}
	if root.code != 0 {
		return "", ErrNotGitRepository
	}
	return filepath.Clean(strings.TrimSpace(root.stdout)), nil
}

func hashPath(path string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return hex.EncodeToString(sum[:16])
}

func coreArgs() []string {
	return []string{"-c", "core.longpaths=true", "-c", "core.symlinks=true"}
}

func cfgArgs() []string {
	return append([]string{"-c", "core.autocrlf=false"}, coreArgs()...)
}

func quoteArgs() []string {
	return append(cfgArgs(), "-c", "core.quotepath=false")
}

func splitNUL(s string) []string {
	parts := strings.Split(s, "\x00")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func lines(s string) []string {
	parts := strings.Split(strings.TrimSpace(s), "\n")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func unique(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func nulList(items []string) []byte {
	if len(items) == 0 {
		return nil
	}
	return []byte(strings.Join(items, "\x00") + "\x00")
}

func clash(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func anyClash(ops []revertOp, rel string) bool {
	for _, op := range ops {
		if clash(op.rel, rel) {
			return true
		}
	}
	return false
}

func removeFile(file string) error {
	err := os.Remove(file)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func parseNumstat(s string) int {
	if s == "-" || s == "" {
		return 0
	}
	var n int
	_, _ = fmt.Sscanf(s, "%d", &n)
	return n
}
