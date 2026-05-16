package fsx

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type TreeNode struct {
	Name     string      `json:"name"`
	Type     string      `json:"type"`
	Children []*TreeNode `json:"children,omitempty"`
}

func DirectoryTree(ctx context.Context, path string, isPathAllowed func(string) error, shouldIgnore func(string) bool, maxItems int) (*TreeNode, error) {
	itemCount := 0
	return directoryTree(ctx, path, isPathAllowed, shouldIgnore, maxItems, &itemCount)
}

func directoryTree(ctx context.Context, path string, isPathAllowed func(string) error, shouldIgnore func(string) bool, maxItems int, itemCount *int) (*TreeNode, error) {
	// Check for context cancellation
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if maxItems > 0 && *itemCount >= maxItems {
		return nil, nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	node := &TreeNode{
		Name: filepath.Base(path),
		Type: "file",
	}

	*itemCount++

	if info.IsDir() {
		node.Type = "directory"
		node.Children = []*TreeNode{}

		entries, err := os.ReadDir(path)
		if err != nil {
			return node, nil // Return partial result on error
		}

		for _, entry := range entries {
			// Check for context cancellation
			select {
			case <-ctx.Done():
				return node, ctx.Err()
			default:
			}

			childPath := filepath.Join(path, entry.Name())
			if err := isPathAllowed(childPath); err != nil {
				continue // Skip disallowed paths
			}

			// Skip if should be ignored (VCS, gitignore, etc.)
			if shouldIgnore != nil && shouldIgnore(childPath) {
				continue
			}

			childNode, err := directoryTree(ctx, childPath, isPathAllowed, shouldIgnore, maxItems, itemCount)
			if err != nil || childNode == nil {
				continue
			}
			node.Children = append(node.Children, childNode)
		}
	}

	return node, nil
}

// CollectFilesFromTree recursively collects file paths from a DirectoryTree.
// Pass basePath="" for relative paths, or a parent directory for absolute paths.
func CollectFilesFromTree(node *TreeNode, basePath string, files *[]string) {
	if node == nil {
		return
	}
	fullPath := filepath.Join(basePath, node.Name)
	switch node.Type {
	case "file":
		*files = append(*files, fullPath)
	case "directory":
		for _, child := range node.Children {
			CollectFilesFromTree(child, fullPath, files)
		}
	}
}

// WalkFilesOptions configures the bounded file walker.
type WalkFilesOptions struct {
	// MaxFiles is the maximum number of files to return (0 = no limit, but defaults to DefaultMaxFiles).
	MaxFiles int
	// MaxDepth is the maximum directory depth to descend (0 = unlimited).
	// Depth 1 means only root directory, depth 2 means root + immediate children, etc.
	MaxDepth int
	// ShouldIgnore is an optional function to filter out paths (return true to skip).
	ShouldIgnore func(path string) bool
}

// DefaultMaxFiles is the default cap for file walking to prevent runaway scans.
const DefaultMaxFiles = 20000

// heavyDirs are directory names that are skipped by default even outside VCS repos.
var heavyDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	".tox":         true,
	"dist":         true,
	"build":        true,
	".cache":       true,
	".gradle":      true,
	".idea":        true,
	".vscode":      true,
}

// allowedDirs are hidden directories that should be traversed despite starting with a dot.
var allowedDirs = map[string]bool{
	".github": true,
	".gitlab": true,
}

// WalkFiles walks the directory tree starting at root and returns a list of file paths.
// It is bounded by MaxFiles (defaults to DefaultMaxFiles) and skips hidden directories
// and known heavy directories like node_modules, vendor, etc.
// The walk respects context cancellation.
func WalkFiles(ctx context.Context, root string, opts WalkFilesOptions) ([]string, error) {
	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = DefaultMaxFiles
	}

	// Clean root path for consistent depth calculation
	root = filepath.Clean(root)
	rootDepth := strings.Count(root, string(filepath.Separator))

	var files []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		// Check context cancellation
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// If we hit the max, stop walking
		if len(files) >= maxFiles {
			return fs.SkipAll
		}

		if err != nil {
			// For root directory errors (like ENOENT), return the error
			if path == root {
				return err
			}
			// Skip subdirectories we can't read
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		// Check depth limit
		if opts.MaxDepth > 0 {
			pathDepth := strings.Count(filepath.Clean(path), string(filepath.Separator)) - rootDepth
			if d.IsDir() && pathDepth >= opts.MaxDepth {
				return fs.SkipDir
			}
		}

		name := d.Name()

		// Skip hidden files/directories (starting with .) except allowed dirs
		if strings.HasPrefix(name, ".") && name != "." {
			if d.IsDir() {
				if !allowedDirs[name] {
					return fs.SkipDir
				}
			} else {
				return nil
			}
		}

		// Skip known heavy directories
		if d.IsDir() && heavyDirs[name] {
			return fs.SkipDir
		}

		// Apply custom ignore function
		if opts.ShouldIgnore != nil && opts.ShouldIgnore(path) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		// Only collect files, not directories
		if !d.IsDir() {
			// Store path relative to root for cleaner display
			relPath, relErr := filepath.Rel(root, path)
			if relErr != nil {
				relPath = path
			}
			files = append(files, relPath)
		}

		return nil
	})

	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return files, err
	}

	return files, nil
}
