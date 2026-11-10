package fsx

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

const AgentsIgnoreFile = ".agentsignore"

type AgentsIgnoreMatcher struct {
	root    string
	matcher gitignore.Matcher
}

func FindAgentsIgnore(startDir string) string {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, AgentsIgnoreFile)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func NewAgentsIgnoreMatcher(startDir string) (*AgentsIgnoreMatcher, error) {
	path := FindAgentsIgnore(startDir)
	if path == "" {
		return nil, nil
	}
	patterns, err := readAgentsIgnorePatterns(path)
	if err != nil {
		return nil, err
	}
	if len(patterns) == 0 {
		return nil, nil
	}
	root := filepath.Dir(path)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	slog.Debug("Loaded .agentsignore", "file", path, "patterns", len(patterns))
	return &AgentsIgnoreMatcher{root: root, matcher: gitignore.NewMatcher(patterns)}, nil
}

func readAgentsIgnorePatterns(path string) ([]gitignore.Pattern, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	lines, err := readIgnoreLines(f)
	if err != nil {
		return nil, err
	}
	patterns := make([]gitignore.Pattern, 0, len(lines))
	for _, line := range lines {
		patterns = append(patterns, gitignore.ParsePattern(line, nil))
	}
	return patterns, nil
}

func ReadAgentsIgnoreGlobs(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	return readIgnoreLines(f)
}

// readIgnoreLines returns the non-empty, non-comment lines of an
// .agentsignore file. Lines are read with bufio.Reader.ReadString so an
// over-long line cannot fail the load on bufio.Scanner's token limit.
func readIgnoreLines(r io.Reader) ([]string, error) {
	var lines []string
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			lines = append(lines, trimmed)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return lines, nil
			}
			return nil, err
		}
	}
}

func (m *AgentsIgnoreMatcher) Match(path string) bool {
	if m == nil {
		return false
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absPath = resolveExisting(absPath)

	relPath, err := filepath.Rel(m.root, absPath)
	if err != nil {
		return false
	}
	normalized := filepath.ToSlash(relPath)
	if normalized == ".." || strings.HasPrefix(normalized, "../") {
		return false
	}
	if normalized == "." {
		return false
	}
	if normalized == AgentsIgnoreFile {
		return true
	}

	info, statErr := os.Stat(absPath)
	isDir := statErr == nil && info.IsDir()
	return m.matcher.Match(strings.Split(normalized, "/"), isDir)
}

func resolveExisting(absPath string) string {
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		return resolved
	}
	dir, rest := filepath.Dir(absPath), filepath.Base(absPath)
	for dir != filepath.Dir(dir) {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, rest)
		}
		dir, rest = filepath.Dir(dir), filepath.Join(filepath.Base(dir), rest)
	}
	return absPath
}

func (m *AgentsIgnoreMatcher) Root() string {
	if m == nil {
		return ""
	}
	return m.root
}
