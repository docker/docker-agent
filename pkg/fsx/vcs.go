package fsx

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// VCSMatcher handles VCS ignore pattern matching for a git repository
type VCSMatcher struct {
	repoRoot string
	matcher  gitignore.Matcher
}

// matcherCache caches VCSMatcher instances by repository root path.
// This avoids repeated .gitignore parsing for the same repository.
var (
	matcherCache   = make(map[string]*VCSMatcher)
	basePathToRoot = make(map[string]string) // maps basePath -> repoRoot for fast lookup
	noRepoCache    = make(map[string]bool)   // paths known to have no repo
	matcherCacheMu sync.RWMutex
)

// NewVCSMatcher creates a new VCS matcher for the given path.
// It searches for a git repository and loads .gitignore patterns.
// Returns (nil, nil) if no git repository is found - this is not an error.
// Results are cached by repository root path to avoid repeated parsing.
func NewVCSMatcher(basePath string) (*VCSMatcher, error) {
	// Quick check: see if we already know this basePath's repo root
	matcherCacheMu.RLock()
	if noRepoCache[basePath] {
		matcherCacheMu.RUnlock()
		return nil, nil
	}
	if repoRoot, ok := basePathToRoot[basePath]; ok {
		if cached, ok := matcherCache[repoRoot]; ok {
			matcherCacheMu.RUnlock()
			slog.Debug("Using cached gitignore patterns", "repository", repoRoot)
			return cached, nil
		}
	}
	matcherCacheMu.RUnlock()

	// A .git entry directly under basePath is the marker for a repository
	// worktree, mirroring the previous go-git PlainOpen behavior (which did
	// not search parent directories either).
	repoRoot, found := findRepoRoot(basePath)
	if !found {
		slog.Debug("No git repository found", "directory", basePath)
		// Cache the negative result
		matcherCacheMu.Lock()
		noRepoCache[basePath] = true
		matcherCacheMu.Unlock()
		return nil, nil
	}

	// Check cache by repo root (read lock)
	matcherCacheMu.RLock()
	if cached, ok := matcherCache[repoRoot]; ok {
		matcherCacheMu.RUnlock()
		// Also cache the basePath -> repoRoot mapping
		matcherCacheMu.Lock()
		basePathToRoot[basePath] = repoRoot
		matcherCacheMu.Unlock()
		slog.Debug("Using cached gitignore patterns", "repository", repoRoot)
		return cached, nil
	}
	matcherCacheMu.RUnlock()

	// Not in cache, need to create (write lock)
	matcherCacheMu.Lock()
	defer matcherCacheMu.Unlock()

	// Double-check after acquiring write lock
	if cached, ok := matcherCache[repoRoot]; ok {
		basePathToRoot[basePath] = repoRoot
		slog.Debug("Using cached gitignore patterns", "repository", repoRoot)
		return cached, nil
	}

	// Read gitignore patterns from the repository
	patterns, err := gitignore.ReadPatterns(osfs.New(repoRoot), nil)
	if err != nil {
		slog.Warn("Failed to read gitignore patterns", "path", repoRoot, "error", err)
		return nil, err
	}

	// Create matcher from patterns
	matcher := gitignore.NewMatcher(patterns)

	slog.Debug("Loaded gitignore patterns", "repository", repoRoot)

	vcsMatcher := &VCSMatcher{
		repoRoot: repoRoot,
		matcher:  matcher,
	}

	// Cache the result
	matcherCache[repoRoot] = vcsMatcher
	basePathToRoot[basePath] = repoRoot

	return vcsMatcher, nil
}

// findRepoRoot reports the git worktree root for basePath. Only basePath
// itself is checked for a .git entry — like the git.PlainOpen call this
// replaces, parent directories are not searched. The entry is lightly
// validated so stray files named .git don't turn a directory into a
// "repository": a .git directory must contain a HEAD file, and a .git
// gitfile (linked worktree / submodule) must carry a "gitdir:" pointer to an
// existing directory. Bare repositories have no .git entry and thus no
// matcher, which is fine: there is no worktree to ignore files in.
//
// The returned root is absolute with symlinks resolved, matching what
// go-billy reported when this used go-git.
func findRepoRoot(basePath string) (string, bool) {
	if !isGitWorktree(basePath) {
		return "", false
	}
	absPath, err := filepath.Abs(basePath)
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		absPath = resolved
	}
	return absPath, true
}

// isGitWorktree reports whether dir/.git denotes a plausible git worktree.
func isGitWorktree(dir string) bool {
	dotGit := filepath.Join(dir, ".git")
	fi, err := os.Stat(dotGit)
	if err != nil {
		return false
	}

	if fi.IsDir() {
		_, err := os.Stat(filepath.Join(dotGit, "HEAD"))
		return err == nil
	}

	// Gitfile: "gitdir: <path>" pointing at the real git dir.
	gitDir, ok := readGitfile(dotGit)
	if !ok {
		return false
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	target, err := os.Stat(gitDir)
	return err == nil && target.IsDir()
}

// readGitfile extracts the "gitdir:" target from a .git gitfile.
func readGitfile(path string) (string, bool) {
	// Gitfiles are one line; cap the read defensively.
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	buf := make([]byte, 4096)
	n, err := f.Read(buf)
	if n == 0 && err != nil {
		return "", false
	}
	content := string(buf[:n])

	const prefix = "gitdir:"
	if !strings.HasPrefix(content, prefix) {
		return "", false
	}
	gitDir, _, _ := strings.Cut(content[len(prefix):], "\n")
	gitDir = strings.TrimSpace(gitDir)
	return gitDir, gitDir != ""
}

// RepoRoot returns the repository root path for this matcher
func (m *VCSMatcher) RepoRoot() string {
	if m == nil {
		return ""
	}
	return m.repoRoot
}

// ShouldIgnore checks if a path should be ignored based on VCS rules.
// It checks both .git directories and .gitignore patterns.
func (m *VCSMatcher) ShouldIgnore(path string) bool {
	if m == nil {
		return false
	}

	// Always ignore .git directories and their contents
	// Check both the original path and normalized path
	base := filepath.Base(path)
	if base == ".git" {
		return true
	}
	normalizedPath := filepath.ToSlash(path)
	if strings.Contains(normalizedPath, "/.git/") || strings.HasSuffix(normalizedPath, "/.git") || strings.HasPrefix(normalizedPath, ".git/") {
		return true
	}

	// Get absolute path
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	// Resolve symlinks to match m.repoRoot, which go-billy now returns with
	// symlinks resolved (e.g. /private/var/... instead of /var/... on macOS).
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		absPath = resolved
	}

	// Create a relative path from the repository root for matching. Rel doubles
	// as the containment check: a path outside the repository climbs out with
	// "..", which a string-prefix test would miss for a sibling directory whose
	// name merely starts with the root's ("<root>-sibling").
	relPath, err := filepath.Rel(m.repoRoot, absPath)
	if err != nil {
		return false
	}
	if relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		return false
	}

	// Check if the path is a directory
	info, err := os.Stat(path)
	isDir := err == nil && info.IsDir()

	normalizedRelPath := filepath.ToSlash(relPath)
	pathComponents := strings.Split(normalizedRelPath, "/")
	matched := m.matcher.Match(pathComponents, isDir)

	return matched
}
