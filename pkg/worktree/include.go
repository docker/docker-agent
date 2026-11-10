package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/docker/docker-agent/pkg/fsx"
)

const worktreeInclude = ".worktreeinclude"

// copyIncludedFiles returns warnings rather than failing after Git has created
// the checkout. Git commands use NUL delimiters to preserve arbitrary names.
func copyIncludedFiles(ctx context.Context, source, workingDir, dest string) []string {
	warn := func(err error) []string { return []string{fmt.Sprintf("%s: %v", worktreeInclude, err)} }
	src, err := os.OpenRoot(source)
	if err != nil {
		return warn(err)
	}
	defer src.Close()
	info, err := src.Lstat(worktreeInclude)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return warn(err)
	}
	if !info.Mode().IsRegular() {
		return warn(errors.New("configuration must be a regular file"))
	}

	matchers := make([]*fsx.AgentsIgnoreMatcher, 0, 2)
	for _, dir := range []string{workingDir, source} {
		matcher, err := fsx.NewAgentsIgnoreMatcher(dir)
		if err != nil {
			return warn(fmt.Errorf("reading .agentsignore: %w", err))
		}
		matchers = append(matchers, matcher)
	}

	candidates, err := gitBytes(ctx, source, nil, "ls-files", "-z", "-o", "-i", "--exclude-from="+worktreeInclude)
	if err != nil {
		return warn(err)
	}
	if len(candidates) == 0 {
		return nil
	}
	ignoredInSource, err := checkIgnoredPaths(ctx, source, candidates)
	if err != nil {
		return warn(err)
	}
	var ignored bytes.Buffer
	for name := range bytes.SplitSeq(candidates, []byte{0}) {
		if ignoredInSource[string(name)] {
			ignored.Write(name)
			ignored.WriteByte(0)
		}
	}
	if ignored.Len() == 0 {
		return nil
	}
	ignoredInDest, err := checkIgnoredPaths(ctx, dest, ignored.Bytes())
	var warnings []string
	failedInDest := make(map[string]bool)
	if err != nil {
		ignoredInDest = make(map[string]bool)
		for name := range bytes.SplitSeq(ignored.Bytes(), []byte{0}) {
			if len(name) == 0 {
				continue
			}
			checked, err := checkIgnoredPaths(ctx, dest, append(bytes.Clone(name), 0))
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("%s: %q: checking destination ignore rules: %v", worktreeInclude, name, err))
				failedInDest[string(name)] = true
				continue
			}
			if checked[string(name)] {
				ignoredInDest[string(name)] = true
			}
		}
	}
	dst, err := os.OpenRoot(dest)
	if err != nil {
		return warn(err)
	}
	defer dst.Close()

	for name := range bytes.SplitSeq(ignored.Bytes(), []byte{0}) {
		if len(name) == 0 {
			continue
		}
		rel := filepath.FromSlash(string(name))
		if !filepath.IsLocal(rel) {
			warnings = append(warnings, fmt.Sprintf("%s: unsafe path %q", worktreeInclude, rel))
			continue
		}
		blocked := filepath.Base(rel) == fsx.AgentsIgnoreFile
		for _, matcher := range matchers {
			if matcher.Match(filepath.Join(source, rel)) {
				blocked = true
			}
		}
		if blocked {
			warnings = append(warnings, worktreeInclude+": skipped a file protected by .agentsignore")
			continue
		}
		if strings.HasSuffix(rel, string(filepath.Separator)) {
			warnings = append(warnings, fmt.Sprintf("%s: skipped nested repository %q", worktreeInclude, rel))
			continue
		}
		if strings.EqualFold(filepath.Base(rel), ".gitignore") || strings.EqualFold(filepath.Base(rel), ".gitattributes") {
			warnings = append(warnings, fmt.Sprintf("%s: %q: Git control files are not copied", worktreeInclude, rel))
			continue
		}
		if failedInDest[string(name)] {
			continue
		}
		if !ignoredInDest[string(name)] {
			warnings = append(warnings, fmt.Sprintf("%s: %q: not ignored in destination; skipped", worktreeInclude, rel))
			continue
		}
		if err := copyIncludedFile(src, dst, rel); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %q: %v", worktreeInclude, rel, err))
		}
	}
	return warnings
}

func checkIgnoredPaths(ctx context.Context, dir string, paths []byte) (map[string]bool, error) {
	var input bytes.Buffer
	for name := range bytes.SplitSeq(paths, []byte{0}) {
		if len(name) > 0 {
			// A leading colon otherwise has Git pathspec-magic semantics.
			input.WriteString("./")
			input.Write(name)
			input.WriteByte(0)
		}
	}
	out, err := gitBytes(ctx, dir, input.Bytes(), "check-ignore", "-z", "--stdin")
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return nil, err
		}
	}
	ignored := make(map[string]bool)
	for name := range bytes.SplitSeq(out, []byte{0}) {
		if trimmed, ok := bytes.CutPrefix(name, []byte("./")); ok {
			ignored[string(trimmed)] = true
		}
	}
	return ignored, nil
}

func gitBytes(ctx context.Context, dir string, input []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// openDirectory rejects symlink components and verifies that each opened
// directory is the one inspected, while os.Root keeps operations contained.
func openDirectory(root *os.Root, parts []string, create bool) (*os.Root, error) {
	current, err := root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	for _, part := range parts {
		info, err := current.Lstat(part)
		if errors.Is(err, fs.ErrNotExist) && create {
			if err = current.Mkdir(part, 0o755); err == nil || errors.Is(err, fs.ErrExist) {
				info, err = current.Lstat(part)
			}
		}
		if err != nil {
			current.Close()
			return nil, err
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			current.Close()
			return nil, fmt.Errorf("directory %q is not a real directory", part)
		}
		next, err := current.OpenRoot(part)
		if err != nil {
			current.Close()
			return nil, err
		}
		opened, err := next.Stat(".")
		current.Close()
		if err != nil {
			next.Close()
			return nil, err
		}
		if !os.SameFile(info, opened) {
			next.Close()
			return nil, fmt.Errorf("directory %q changed while opening", part)
		}
		current = next
	}
	return current, nil
}

func copyIncludedFile(src, dst *os.Root, rel string) error {
	if !filepath.IsLocal(rel) {
		return fmt.Errorf("unsafe path %q", rel)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	name := parts[len(parts)-1]
	srcDir, err := openDirectory(src, parts[:len(parts)-1], false)
	if err != nil {
		return err
	}
	defer srcDir.Close()
	info, err := srcDir.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	in, err := srcDir.OpenFile(name, os.O_RDONLY|nonblockFlag, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return errors.New("source changed while opening")
	}

	dstDir, err := openDirectory(dst, parts[:len(parts)-1], true)
	if err != nil {
		return err
	}
	defer dstDir.Close()
	out, err := dstDir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	if copyErr == nil {
		copyErr = out.Chmod(info.Mode().Perm())
	}
	if err := out.Close(); copyErr == nil {
		copyErr = err
	}
	if copyErr != nil {
		_ = dstDir.Remove(name)
	}
	return copyErr
}
