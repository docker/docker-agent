package fsx

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5"
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

	// PlainOpen automatically searches up the directory tree for .git
	repo, err := git.PlainOpen(basePath)
	if err != nil {
		slog.Debug("No git repository found", "directory", basePath)
		// Cache the negative result
		matcherCacheMu.Lock()
		noRepoCache[basePath] = true
		matcherCacheMu.Unlock()
		return nil, nil
	}

	// Get the worktree
	worktree, err := repo.Worktree()
	if err != nil {
		slog.Warn("Failed to get worktree", "path", basePath, "error", err)
		return nil, err
	}

	// Get the repository root path
	repoRoot := worktree.Filesystem.Root()

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
	patterns, err := gitignore.ReadPatterns(worktree.Filesystem, nil)
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

	// Check if path is within this repository
	if !strings.HasPrefix(absPath, m.repoRoot) {
		return false
	}

	// Create a relative path from the repository root for matching
	relPath, err := filepath.Rel(m.repoRoot, absPath)
	if err != nil {
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
