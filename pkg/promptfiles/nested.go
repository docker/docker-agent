package promptfiles

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/fsx"
)

// MaxIndexEntries caps how many nested prompt files [Index] lists so a
// monorepo with hundreds of sub-projects can't flood the context.
const MaxIndexEntries = 100

// maxScanEntries bounds the walk itself: the entry cap above only stops the
// scan once enough matches were found, and this runs on every turn.
const maxScanEntries = 20_000

// Index lists, by path only, the files named by filenames that live below
// workDir — up to maxDepth directory levels down, 1 being the direct
// subdirectories. Contents are deliberately not read: the listing costs
// tokens proportional to the number of sub-projects rather than to the size
// of their instructions, and the agent reads the ones it needs itself.
//
// Paths in loaded are skipped, so a file is never both injected and listed.
// Returns "" when maxDepth <= 0 (the scan is opt-in) or nothing is found.
func Index(ctx context.Context, workDir string, filenames []string, maxDepth int, loaded []string) string {
	root, err := filepath.Abs(workDir)
	if err != nil {
		return ""
	}
	// filepath.WalkDir doesn't descend into a symlinked root. Resolving it
	// also makes every path it yields canonical, which the loaded-set and
	// containment checks below compare against.
	root = resolve(root)

	found, truncated := nested(ctx, root, filenames, maxDepth, loaded)
	if len(found) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Prompt files found in subdirectories of %s. Their contents are NOT loaded:\n\n", root)
	for _, path := range found {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		fmt.Fprintf(&b, "- %s\n", filepath.ToSlash(rel))
	}
	if truncated {
		b.WriteString("\n(listing truncated; more prompt files exist below this directory)\n")
	}
	b.WriteString("\nRead the ones covering the files you are about to work on. For anything under its own directory, a nested file's instructions take precedence over the instructions loaded above.")
	return b.String()
}

// nested walks up to maxDepth levels below root — which must be absolute and
// symlink-free — and returns the paths of every file named after one of
// filenames, excluding loaded. Hidden and git-ignored directories are
// skipped. truncated reports a walk cut short by [MaxIndexEntries],
// [maxScanEntries] or a cancelled ctx; the entries collected so far are still
// the lexically first ones, since [filepath.WalkDir] visits in order.
func nested(ctx context.Context, root string, filenames []string, maxDepth int, loaded []string) (paths []string, truncated bool) {
	if maxDepth <= 0 || len(filenames) == 0 {
		return nil, false
	}
	loadedSet := make(map[string]bool, len(loaded))
	for _, path := range loaded {
		loadedSet[resolve(path)] = true
	}
	// Best-effort .gitignore filtering: nil (a no-op matcher) when root isn't
	// a git worktree root, leaving only the hidden-dir skip.
	ignore, _ := fsx.NewVCSMatcher(root)

	scanned := 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return nil
		}
		scanned++
		if scanned > maxScanEntries || ctx.Err() != nil {
			truncated = true
			return fs.SkipAll
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		// Levels between root and the entry's parent: 0 directly inside root.
		depth := strings.Count(filepath.ToSlash(rel), "/")

		if d.IsDir() {
			// Entries inside sit one level deeper than the directory itself.
			if strings.HasPrefix(d.Name(), ".") || depth >= maxDepth || ignore.ShouldIgnore(path) {
				return fs.SkipDir
			}
			return nil
		}
		if depth == 0 || !slices.Contains(filenames, d.Name()) || ignore.ShouldIgnore(path) {
			return nil
		}
		// A control character in a path would break out of the rendered
		// bullet list and let a checked-out tree inject arbitrary text into
		// the system prompt.
		if strings.ContainsFunc(rel, func(r rune) bool { return r < ' ' || r == 0x7f }) {
			slog.DebugContext(ctx, "skipping prompt file with control characters in its path", "path", path)
			return nil
		}

		target := path
		switch {
		case d.Type().IsRegular():
		case d.Type()&fs.ModeSymlink != 0:
			// Listing a symlink that escapes the tree would invite the agent
			// to read an arbitrary file as project instructions.
			target = resolve(path)
			if !isFile(target) || !isUnder(target, root) {
				return nil
			}
		default:
			return nil
		}
		if loadedSet[target] {
			return nil
		}

		if len(paths) == MaxIndexEntries {
			truncated = true
			return fs.SkipAll
		}
		paths = append(paths, path)
		return nil
	})
	return paths, truncated
}

// resolve canonicalizes path, falling back to it unchanged when resolution
// fails (broken symlink, unreadable parent).
func resolve(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// isUnder reports whether path is contained in root. Both must be absolute
// and symlink-free for the comparison to mean anything.
func isUnder(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
