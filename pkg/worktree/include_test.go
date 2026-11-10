package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func includeFile(t *testing.T, dir, name, content string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
}

func TestWorktreeIncludeSelection(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	includeFile(t, dir, ".gitignore", "*.env\ncache/\n*.key\n", 0o644)
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "commit", "-m", "ignore local files")
	includeFile(t, dir, ".worktreeinclude", "# comment\n*.env\n!skip.env\n/cache/\n*.key\n", 0o644)
	includeFile(t, dir, ".agentsignore", "private.key\n", 0o644)
	includeFile(t, dir, "local.env", "local", 0o600)
	includeFile(t, dir, "skip.env", "skip", 0o600)
	includeFile(t, dir, "ordinary.txt", "unignored", 0o600)
	name := "line\nbreak"
	if runtime.GOOS == "windows" {
		name = "line-break"
	}
	includeFile(t, dir, filepath.Join("cache", "nested", name), "nested", 0o640)
	includeFile(t, dir, "private.key", "secret", 0o600)
	includeFile(t, dir, "public.key", "public", 0o600)
	wt, err := Create(t.Context(), dir, "included", WithRoot(t.TempDir()))
	require.NoError(t, err)
	assert.Equal(t, []string{`.worktreeinclude: skipped a file protected by .agentsignore`}, wt.Warnings)
	for name, expected := range map[string]string{"local.env": "local", filepath.Join("cache", "nested", name): "nested", "public.key": "public"} {
		content, err := os.ReadFile(filepath.Join(wt.Dir, name))
		require.NoError(t, err)
		assert.Equal(t, expected, string(content))
	}
	for _, name := range []string{"skip.env", "ordinary.txt", "private.key"} {
		assert.NoFileExists(t, filepath.Join(wt.Dir, name))
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(wt.Dir, "cache", "nested", name))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestWorktreeIncludeDoesNotOverwriteBase(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	includeFile(t, dir, "local.env", "tracked base", 0o600)
	runGit(t, dir, "add", "local.env")
	runGit(t, dir, "commit", "-m", "base file")
	runGit(t, dir, "branch", "with-file")
	runGit(t, dir, "rm", "local.env")
	runGit(t, dir, "commit", "-m", "remove file")
	includeFile(t, dir, ".gitignore", "*.env\n", 0o644)
	includeFile(t, dir, ".worktreeinclude", "*.env\n", 0o644)
	includeFile(t, dir, "local.env", "source", 0o600)
	wt, err := Create(t.Context(), dir, "collision", WithRoot(t.TempDir()), WithBase("with-file"))
	require.NoError(t, err)
	content, err := os.ReadFile(filepath.Join(wt.Dir, "local.env"))
	require.NoError(t, err)
	assert.Equal(t, "tracked base", string(content))
	assert.Len(t, wt.Warnings, 1)
}

func TestWorktreeIncludeSourceAgentsIgnore(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	includeFile(t, dir, ".gitignore", "*.env\n", 0o644)
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "commit", "-m", "ignore env files")
	includeFile(t, dir, ".worktreeinclude", "*.env\n", 0o644)
	includeFile(t, dir, ".agentsignore", "root.env\n", 0o644)
	includeFile(t, dir, "sub/.agentsignore", "/sub.env\n", 0o644)
	includeFile(t, dir, "root.env", "root secret", 0o600)
	includeFile(t, dir, "sub/sub.env", "sub secret", 0o600)
	includeFile(t, dir, "safe.env", "safe", 0o600)
	wt, err := Create(t.Context(), filepath.Join(dir, "sub"), "protected", WithRoot(t.TempDir()))
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(wt.Dir, "root.env"))
	assert.NoFileExists(t, filepath.Join(wt.Dir, "sub", "sub.env"))
	assert.FileExists(t, filepath.Join(wt.Dir, "safe.env"))
	assert.Equal(t, []string{`.worktreeinclude: skipped 2 files protected by .agentsignore`}, wt.Warnings)
}

func TestWorktreeIncludeDoesNotCopyAgentsIgnoreVariants(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	includeFile(t, dir, ".gitignore", ".AGENTSIGNORE\nnested/\n*.env\n", 0o644)
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "commit", "-m", "ignore local files")
	includeFile(t, dir, ".worktreeinclude", ".AGENTSIGNORE\nnested/\n*.env\n", 0o644)
	includeFile(t, dir, ".AGENTSIGNORE", "", 0o644)
	includeFile(t, dir, "nested/.AgEnTsIgNoRe", "", 0o644)
	includeFile(t, dir, "safe.env", "safe", 0o600)

	wt, err := Create(t.Context(), dir, "ignore-variants", WithRoot(t.TempDir()))
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(wt.Dir, ".AGENTSIGNORE"))
	assert.NoFileExists(t, filepath.Join(wt.Dir, "nested", ".AgEnTsIgNoRe"))
	content, err := os.ReadFile(filepath.Join(wt.Dir, "safe.env"))
	require.NoError(t, err)
	assert.Equal(t, "safe", string(content))
	assert.Equal(t, []string{`.worktreeinclude: skipped 2 files protected by .agentsignore`}, wt.Warnings)
}

func TestWorktreeIncludeUppercaseActiveAgentsIgnore(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	includeFile(t, dir, ".gitignore", ".AGENTSIGNORE\n*.env\n", 0o644)
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "commit", "-m", "ignore local files")
	includeFile(t, dir, ".worktreeinclude", ".AGENTSIGNORE\n*.env\n", 0o644)
	includeFile(t, dir, ".AGENTSIGNORE", "private.env\n", 0o644)
	if _, err := os.Stat(filepath.Join(dir, ".agentsignore")); os.IsNotExist(err) {
		t.Skip("filesystem is case-sensitive")
	} else {
		require.NoError(t, err)
	}
	includeFile(t, dir, "private.env", "secret", 0o600)
	includeFile(t, dir, "safe.env", "safe", 0o600)

	wt, err := Create(t.Context(), dir, "uppercase-policy", WithRoot(t.TempDir()))
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(wt.Dir, ".AGENTSIGNORE"))
	assert.NoFileExists(t, filepath.Join(wt.Dir, "private.env"))
	content, err := os.ReadFile(filepath.Join(wt.Dir, "safe.env"))
	require.NoError(t, err)
	assert.Equal(t, "safe", string(content))
	assert.Equal(t, []string{`.worktreeinclude: skipped 2 files protected by .agentsignore`}, wt.Warnings)
}

func TestWorktreeIncludeUnsafePaths(t *testing.T) {
	t.Parallel()
	source, dest, outside := t.TempDir(), t.TempDir(), t.TempDir()
	includeFile(t, source, "file", "source", 0o600)
	includeFile(t, outside, "file", "outside", 0o600)
	require.NoError(t, os.Mkdir(filepath.Join(source, "directory"), 0o755))
	if err := os.Symlink(outside, filepath.Join(source, "link")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	require.NoError(t, os.Symlink(filepath.Join(outside, "file"), filepath.Join(source, "linked-file")))
	require.NoError(t, os.Symlink(outside, filepath.Join(dest, "directory")))
	src, err := os.OpenRoot(source)
	require.NoError(t, err)
	defer src.Close()
	dst, err := os.OpenRoot(dest)
	require.NoError(t, err)
	defer dst.Close()
	for _, path := range []string{"../file", "link/file", "linked-file", "directory/file"} {
		t.Run(path, func(t *testing.T) { require.Error(t, copyIncludedFile(t.Context(), src, dst, path)) })
	}
	content, err := os.ReadFile(filepath.Join(outside, "file"))
	require.NoError(t, err)
	assert.Equal(t, "outside", string(content))
}

func TestWorktreeIncludeWarningsDoNotLoseWorktree(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".worktreeinclude"), 0o755))
	wt, err := Create(t.Context(), dir, "bad-config", WithRoot(t.TempDir()))
	require.NoError(t, err)
	assert.DirExists(t, wt.Dir)
	require.Len(t, wt.Warnings, 1)
	assert.Contains(t, wt.Warnings[0], "regular file")
}

func TestWorktreeIncludePR(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	dir := bootstrapRepo(t)
	includeFile(t, dir, ".gitignore", "*.env\n", 0o644)
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "commit", "-m", "ignore env files")
	includeFile(t, dir, ".worktreeinclude", "*.env\n", 0o644)
	includeFile(t, dir, "local.env", "present", 0o600)
	bin := t.TempDir()
	includeFile(t, bin, "gh", "#!/bin/sh\ngit checkout -b pr-test\n", 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	wt, err := CreatePR(t.Context(), dir, "123", WithRoot(t.TempDir()))
	require.NoError(t, err)
	assert.Empty(t, wt.Warnings)
	content, err := os.ReadFile(filepath.Join(wt.Dir, "local.env"))
	require.NoError(t, err)
	assert.Equal(t, "present", string(content))
}

func TestWorktreeIncludeDestinationIgnore(t *testing.T) {
	for _, kind := range []string{"base", "pr"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "pr" && runtime.GOOS == "windows" {
				t.Skip("shell script")
			}
			dir := bootstrapRepo(t)
			includeFile(t, dir, ".gitignore", "*.env\n!exposed*.env\n!:/*.env\n", 0o644)
			runGit(t, dir, "add", ".gitignore")
			runGit(t, dir, "commit", "-m", "destination ignore rules")
			runGit(t, dir, "branch", "destination")
			sibling := filepath.Join(t.TempDir(), "sibling")
			runGit(t, dir, "worktree", "add", "--detach", sibling)
			includeFile(t, dir, ".gitignore", "*.env\n*.key\n", 0o644)
			includeFile(t, dir, ".worktreeinclude", "*.env\n*.key\n", 0o644)
			copied := []string{"safe.env", "nested/safe.env"}
			skipped := []string{"exposed.env", "local.key"}
			if runtime.GOOS != "windows" {
				copied = append(copied, "[safe]*?.env", "line\nbreak.env", "# ! trailing .env")
				skipped = append(skipped, "exposed[abc]*?.env", "[unsafe]*?.key", "line\nbreak.key", ":/safe.env")
			}
			for _, names := range [][]string{copied, skipped} {
				for _, name := range names {
					includeFile(t, dir, name, "secret", 0o600)
				}
			}
			configPath := gitOut(t, dir, "rev-parse", "--git-path", "config")
			excludePath := gitOut(t, dir, "rev-parse", "--git-path", "info/exclude")
			if !filepath.IsAbs(configPath) {
				configPath = filepath.Join(dir, configPath)
			}
			if !filepath.IsAbs(excludePath) {
				excludePath = filepath.Join(dir, excludePath)
			}
			configBefore, err := os.ReadFile(configPath)
			require.NoError(t, err)
			excludeBefore, err := os.ReadFile(excludePath)
			require.NoError(t, err)
			if kind == "pr" {
				bin := t.TempDir()
				includeFile(t, bin, "gh", "#!/bin/sh\ngit checkout destination\n", 0o755)
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			var wt *Worktree
			if kind == "base" {
				wt, err = Create(t.Context(), dir, "destination-test", WithRoot(t.TempDir()), WithBase("destination"))
			} else {
				wt, err = CreatePR(t.Context(), dir, "123", WithRoot(t.TempDir()))
			}
			require.NoError(t, err)
			assert.Len(t, wt.Warnings, len(skipped))
			for _, warning := range wt.Warnings {
				assert.Contains(t, warning, "not ignored in destination; skipped")
			}
			for _, name := range copied {
				assert.FileExists(t, filepath.Join(wt.Dir, name))
				out, err := gitBytes(t.Context(), wt.Dir, []byte("./"+filepath.ToSlash(name)+"\x00"), "check-ignore", "-z", "--stdin")
				require.NoError(t, err, name)
				assert.Equal(t, "./"+filepath.ToSlash(name)+"\x00", string(out))
			}
			for _, name := range skipped {
				assert.NoFileExists(t, filepath.Join(wt.Dir, name))
				_, err := gitBytes(t.Context(), wt.Dir, []byte("./"+filepath.ToSlash(name)+"\x00"), "check-ignore", "-z", "--stdin")
				require.Error(t, err, name)
				_, err = gitBytes(t.Context(), sibling, []byte("./"+filepath.ToSlash(name)+"\x00"), "check-ignore", "-z", "--stdin")
				require.Error(t, err, name)
				_, err = gitBytes(t.Context(), dir, []byte("./"+filepath.ToSlash(name)+"\x00"), "check-ignore", "-z", "--stdin")
				require.NoError(t, err, name)
			}
			assert.Empty(t, gitOut(t, wt.Dir, "status", "--porcelain"))
			runGit(t, wt.Dir, "add", "-A")
			assert.Empty(t, gitOut(t, wt.Dir, "diff", "--cached", "--name-only"))
			configAfter, err := os.ReadFile(configPath)
			require.NoError(t, err)
			assert.Equal(t, configBefore, configAfter)
			excludeAfter, err := os.ReadFile(excludePath)
			require.NoError(t, err)
			assert.Equal(t, excludeBefore, excludeAfter)
		})
	}
}

func TestWorktreeIncludeSourcePathspecMagic(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("colon in filename")
	}
	dir := bootstrapRepo(t)
	includeFile(t, dir, ".gitignore", "*.env\n", 0o644)
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "commit", "-m", "destination ignore rules")
	runGit(t, dir, "branch", "destination")
	includeFile(t, dir, ".gitignore", "*.env\n!:/*.env\n", 0o644)
	includeFile(t, dir, ".worktreeinclude", "*.env\n", 0o644)
	includeFile(t, dir, ":/safe.env", "not ignored in source", 0o600)
	wt, err := Create(t.Context(), dir, "source-magic", WithRoot(t.TempDir()), WithBase("destination"))
	require.NoError(t, err)
	assert.Empty(t, wt.Warnings)
	assert.NoFileExists(t, filepath.Join(wt.Dir, ":", "safe.env"))
}

func TestWorktreeIncludeDoesNotCopyIgnoreFiles(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	includeFile(t, dir, ".gitignore", "nested/\n", 0o644)
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "commit", "-m", "ignore nested files")
	includeFile(t, dir, ".worktreeinclude", "nested/\n", 0o644)
	includeFile(t, dir, "nested/.gitignore", "!secret.env\n", 0o644)
	includeFile(t, dir, "nested/.gitattributes", "secret.env !export-ignore\n", 0o644)
	includeFile(t, dir, "nested/secret.env", "secret", 0o600)
	wt, err := Create(t.Context(), dir, "ignore-file", WithRoot(t.TempDir()))
	require.NoError(t, err)
	require.Len(t, wt.Warnings, 2)
	for _, warning := range wt.Warnings {
		assert.Contains(t, warning, "Git control files are not copied")
	}
	assert.NoFileExists(t, filepath.Join(wt.Dir, "nested", ".gitignore"))
	assert.NoFileExists(t, filepath.Join(wt.Dir, "nested", ".gitattributes"))
	assert.FileExists(t, filepath.Join(wt.Dir, "nested", "secret.env"))
	runGit(t, wt.Dir, "check-ignore", "nested/secret.env")
	assert.Empty(t, gitOut(t, wt.Dir, "status", "--porcelain"))
	runGit(t, wt.Dir, "add", "-A")
	assert.Empty(t, gitOut(t, wt.Dir, "diff", "--cached", "--name-only"))
}

func TestWorktreeIncludeDestinationSymlinkDoesNotBlockOtherFiles(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	includeFile(t, dir, ".gitignore", "*.env\n", 0o644)
	includeFile(t, dir, "link-target", "target", 0o600)
	if err := os.Symlink("link-target", filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	runGit(t, dir, "add", ".gitignore", "link", "link-target")
	runGit(t, dir, "commit", "-m", "destination symlink")
	runGit(t, dir, "branch", "destination-symlink")
	require.NoError(t, os.Remove(filepath.Join(dir, "link")))
	includeFile(t, dir, "link/local.env", "secret", 0o600)
	includeFile(t, dir, "safe.env", "safe", 0o600)
	for i := range 64 {
		includeFile(t, dir, fmt.Sprintf("cache/file-%03d.env", i), "cached", 0o600)
	}
	includeFile(t, dir, ".worktreeinclude", "*.env\n", 0o644)
	wt, err := Create(t.Context(), dir, "symlink-dest", WithRoot(t.TempDir()), WithBase("destination-symlink"))
	require.NoError(t, err)
	require.Len(t, wt.Warnings, 1)
	assert.Contains(t, wt.Warnings[0], "checking destination ignore rules")
	assert.FileExists(t, filepath.Join(wt.Dir, "safe.env"))
	for i := range 64 {
		content, err := os.ReadFile(filepath.Join(wt.Dir, fmt.Sprintf("cache/file-%03d.env", i)))
		require.NoError(t, err)
		assert.Equal(t, "cached", string(content))
	}
	assert.Empty(t, gitOut(t, wt.Dir, "status", "--porcelain"))
	runGit(t, wt.Dir, "add", "-A")
	assert.Empty(t, gitOut(t, wt.Dir, "diff", "--cached", "--name-only"))
}

func TestCheckDestinationPathsGitBatchSplitting(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	includeFile(t, dir, ".gitignore", "*.env\n!exposed.env\n", 0o644)
	includeFile(t, dir, "target", "target", 0o600)
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	var paths bytes.Buffer
	for i := range 512 {
		fmt.Fprintf(&paths, "file-%03d.env\x00", i)
	}
	paths.WriteString("link/bad.env\x00exposed.env\x00")
	calls := 0
	check := func(ctx context.Context, dir string, paths []byte) (map[string]bool, error) {
		calls++
		return checkIgnoredPaths(ctx, dir, paths)
	}
	var warnings includeWarnings
	ignored, failed := checkDestinationPaths(t.Context(), dir, paths.Bytes(), check, &warnings)
	assert.LessOrEqual(t, calls, 21)
	assert.Len(t, ignored, 512)
	for i := range 512 {
		assert.True(t, ignored[fmt.Sprintf("file-%03d.env", i)])
	}
	assert.False(t, ignored["exposed.env"])
	assert.Equal(t, map[string]bool{"link/bad.env": true}, failed)
	require.Len(t, warnings.entries, 1)
	assert.Contains(t, warnings.entries[0], `"link/bad.env": checking destination ignore rules`)
}

func TestCheckDestinationPathsFailures(t *testing.T) {
	t.Parallel()
	paths := []byte("good\x00bad-one\x00line\nbreak\x00bad-two\x00unignored\x00")
	var warnings includeWarnings
	calls := 0
	check := func(_ context.Context, _ string, input []byte) (map[string]bool, error) {
		calls++
		ignored := make(map[string]bool)
		for name := range bytes.SplitSeq(input, []byte{0}) {
			if bytes.HasPrefix(name, []byte("bad-")) {
				return map[string]bool{"must-not-copy": true}, errors.New("collision")
			}
			if len(name) > 0 && string(name) != "unignored" {
				ignored[string(name)] = true
			}
		}
		return ignored, nil
	}
	ignored, failed := checkDestinationPaths(t.Context(), "", paths, check, &warnings)
	assert.Less(t, calls, 2*5)
	assert.Equal(t, map[string]bool{"good": true, "line\nbreak": true}, ignored)
	assert.Equal(t, map[string]bool{"bad-one": true, "bad-two": true}, failed)
	assert.Equal(t, []string{
		`.worktreeinclude: "bad-one": checking destination ignore rules: collision`,
		`.worktreeinclude: "bad-two": checking destination ignore rules: collision`,
	}, warnings.entries)
}

func TestCheckDestinationPathsGlobalError(t *testing.T) {
	t.Parallel()
	var warnings includeWarnings
	calls := 0
	check := func(_ context.Context, _ string, _ []byte) (map[string]bool, error) {
		calls++
		return map[string]bool{"a": true}, errors.New("global failure")
	}
	ignored, failed := checkDestinationPaths(t.Context(), "", []byte("a\x00b\x00c\x00"), check, &warnings)
	assert.Equal(t, 5, calls)
	assert.Empty(t, ignored)
	assert.Equal(t, map[string]bool{"a": true, "b": true, "c": true}, failed)
	assert.Equal(t, []string{
		`.worktreeinclude: "a": checking destination ignore rules: global failure`,
		`.worktreeinclude: "b": checking destination ignore rules: global failure`,
		`.worktreeinclude: "c": checking destination ignore rules: global failure`,
	}, warnings.entries)
}

func TestCheckDestinationPathsBoundedFailures(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"shared conflict", "global error"} {
		t.Run(scenario, func(t *testing.T) {
			const count = 4096
			var paths bytes.Buffer
			for i := range count {
				fmt.Fprintf(&paths, "link/file-%04d.env\x00", i)
			}
			calls := 0
			check := func(ctx context.Context, dir string, input []byte) (map[string]bool, error) {
				calls++
				if scenario == "shared conflict" {
					return checkIgnoredPaths(ctx, dir, input)
				}
				return map[string]bool{"link/file-0000.env": true}, errors.New("global failure")
			}
			dir := t.TempDir()
			if scenario == "shared conflict" {
				dir = bootstrapRepo(t)
				includeFile(t, dir, ".gitignore", "*.env\n", 0o644)
				includeFile(t, dir, "target", "target", 0o600)
				if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
					t.Skipf("symlink: %v", err)
				}
			}
			var warnings includeWarnings
			for range maxIncludeWarnings {
				warnings.add("earlier warning")
			}
			warnings.protected = 1
			warnings.cancelled = "cancellation warning"
			ignored, failed := checkDestinationPaths(t.Context(), dir, paths.Bytes(), check, &warnings)
			if scenario == "shared conflict" {
				assert.Equal(t, 1, calls)
			} else {
				assert.Equal(t, maxDestinationChecks, calls)
			}
			assert.Empty(t, ignored)
			assert.Len(t, failed, count)
			for i := range count {
				assert.True(t, failed[fmt.Sprintf("link/file-%04d.env", i)])
			}
			result := warnings.result()
			assert.LessOrEqual(t, len(result), maxIncludeWarnings)
			joined := strings.Join(result, "\n")
			assert.Contains(t, joined, "additional warnings omitted")
			assert.Contains(t, joined, "protected by .agentsignore")
			assert.Contains(t, joined, "cancellation warning")
			if scenario == "global error" {
				assert.Contains(t, joined, "destination ignore check limit reached; 4096 files skipped without verification")
				assert.Contains(t, joined, "global failure")
			} else {
				assert.Empty(t, warnings.checkLimit)
			}
		})
	}
}

func TestCheckDestinationPathsBoundedMixedFailures(t *testing.T) {
	t.Parallel()
	var paths bytes.Buffer
	for i := range 4096 {
		fmt.Fprintf(&paths, "link/file-%04d.env\x00", i)
	}
	for i := range 4096 {
		fmt.Fprintf(&paths, "safe/file-%04d.env\x00", i)
	}
	paths.WriteString("exposed.env\x00")
	calls := 0
	check := func(_ context.Context, _ string, input []byte) (map[string]bool, error) {
		calls++
		if bytes.Contains(input, []byte("link/")) {
			return map[string]bool{"link/file-0000.env": true}, errors.New("collision")
		}
		ignored := make(map[string]bool)
		for name := range bytes.SplitSeq(input, []byte{0}) {
			if bytes.HasPrefix(name, []byte("safe/")) {
				ignored[string(name)] = true
			}
		}
		return ignored, nil
	}
	var warnings includeWarnings
	ignored, failed := checkDestinationPaths(t.Context(), "", paths.Bytes(), check, &warnings)
	assert.Equal(t, maxDestinationChecks, calls)
	assert.Len(t, ignored, 4096)
	for i := range 4096 {
		assert.True(t, ignored[fmt.Sprintf("safe/file-%04d.env", i)])
		assert.True(t, failed[fmt.Sprintf("link/file-%04d.env", i)])
	}
	assert.False(t, failed["exposed.env"])
	assert.False(t, ignored["exposed.env"])
	assert.Contains(t, warnings.checkLimit, "without verification: collision")
}

func TestCheckDestinationPathsUnevenAndSparseGitConflicts(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	includeFile(t, dir, ".gitignore", "*.env\n!exposed.env\n", 0o644)
	includeFile(t, dir, "target", "target", 0o600)
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	var paths bytes.Buffer
	for i := range 100 {
		fmt.Fprintf(&paths, "a/%04d.env\x00", i)
	}
	for i := range 4096 {
		fmt.Fprintf(&paths, "link/%04d.env\x00", i)
	}
	for i := range 100 {
		fmt.Fprintf(&paths, "z/%04d.env\x00", i)
	}
	for i := range 64 {
		fmt.Fprintf(&paths, "sparse-%04d.env\x00link/sparse-%04d.env\x00", i, i)
	}
	paths.WriteString("exposed.env\x00")
	calls := 0
	check := func(ctx context.Context, dir string, paths []byte) (map[string]bool, error) {
		calls++
		return checkIgnoredPaths(ctx, dir, paths)
	}
	var warnings includeWarnings
	ignored, failed := checkDestinationPaths(t.Context(), dir, paths.Bytes(), check, &warnings)
	assert.Equal(t, 2, calls)
	assert.Len(t, ignored, 264)
	assert.Len(t, failed, 4160)
	for i := range 100 {
		assert.True(t, ignored[fmt.Sprintf("a/%04d.env", i)])
		assert.True(t, ignored[fmt.Sprintf("z/%04d.env", i)])
	}
	for i := range 64 {
		assert.True(t, ignored[fmt.Sprintf("sparse-%04d.env", i)])
		assert.True(t, failed[fmt.Sprintf("link/sparse-%04d.env", i)])
	}
	assert.False(t, ignored["exposed.env"])
	assert.False(t, failed["exposed.env"])
	assert.Empty(t, warnings.checkLimit)
}

func TestCheckDestinationPathsCancellationAtLimit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var paths bytes.Buffer
	for i := range 4096 {
		fmt.Fprintf(&paths, "file-%04d\x00", i)
	}
	calls := 0
	check := func(_ context.Context, _ string, _ []byte) (map[string]bool, error) {
		calls++
		if calls == maxDestinationChecks {
			cancel()
		}
		return map[string]bool{"file-0000": true}, errors.New("failure")
	}
	var warnings includeWarnings
	ignored, failed := checkDestinationPaths(ctx, "", paths.Bytes(), check, &warnings)
	assert.Equal(t, maxDestinationChecks, calls)
	assert.Empty(t, ignored)
	assert.Empty(t, failed)
	assert.Empty(t, warnings.checkLimit)
	assert.ErrorIs(t, ctx.Err(), context.Canceled)
}

func TestCheckDestinationPathsCancellation(t *testing.T) {
	t.Parallel()
	for _, cancelAt := range []int{0, 1, 2} {
		t.Run(strconv.Itoa(cancelAt), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			if cancelAt == 0 {
				cancel()
			}
			check := func(_ context.Context, _ string, _ []byte) (map[string]bool, error) {
				calls++
				if calls == cancelAt {
					cancel()
				}
				return nil, errors.New("failure")
			}
			var warnings includeWarnings
			ignored, failed := checkDestinationPaths(ctx, "", []byte("a\x00b\x00c\x00d\x00"), check, &warnings)
			assert.Equal(t, cancelAt, calls)
			assert.Empty(t, ignored)
			assert.Empty(t, failed)
			assert.Empty(t, warnings.entries)
			assert.ErrorIs(t, ctx.Err(), context.Canceled)
		})
	}
}

func TestWorktreeIncludeDestinationCheckFailure(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	includeFile(t, dir, ".gitignore", "*.env\n", 0o644)
	includeFile(t, dir, ".worktreeinclude", "*.env\n", 0o644)
	includeFile(t, dir, "local.env", "secret", 0o600)
	dest := t.TempDir()
	warnings := copyIncludedFiles(t.Context(), dir, dir, dest)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "checking destination ignore rules")
	assert.NoFileExists(t, filepath.Join(dest, "local.env"))
}

func TestWorktreeIncludePrivateDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	t.Parallel()
	for _, existing := range []bool{false, true} {
		t.Run(strconv.FormatBool(existing), func(t *testing.T) {
			dir := bootstrapRepo(t)
			require.NoError(t, os.Chmod(dir, 0o755))
			includeFile(t, dir, ".gitignore", "*.env\n", 0o644)
			if existing {
				includeFile(t, dir, "private/tracked", "tracked", 0o644)
			}
			runGit(t, dir, "add", ".gitignore")
			if existing {
				runGit(t, dir, "add", "private/tracked")
			}
			runGit(t, dir, "commit", "-m", "ignore local files")
			includeFile(t, dir, ".worktreeinclude", "*.env\n", 0o644)
			includeFile(t, dir, "private/secret.env", "secret", 0o644)
			require.NoError(t, os.Chmod(filepath.Join(dir, "private"), 0o700))
			root := t.TempDir()
			wt, err := Create(t.Context(), dir, "private", WithRoot(root))
			require.NoError(t, err)
			require.NoError(t, os.Chmod(wt.Dir, 0o755))
			assert.Empty(t, wt.Warnings)
			parent, err := os.Stat(filepath.Join(wt.Dir, "private"))
			require.NoError(t, err)
			file, err := os.Stat(filepath.Join(wt.Dir, "private", "secret.env"))
			require.NoError(t, err)
			if existing {
				assert.Equal(t, os.FileMode(0), file.Mode().Perm()&0o077)
			} else {
				assert.Equal(t, os.FileMode(0), parent.Mode().Perm()&0o077)
			}
		})
	}
}

func TestWorktreeIncludeProtectedDestinationSymlink(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	includeFile(t, dir, ".gitignore", "*.env\n", 0o644)
	includeFile(t, dir, "target", "target", 0o600)
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	runGit(t, dir, "add", ".gitignore", "link", "target")
	runGit(t, dir, "commit", "-m", "destination symlink")
	runGit(t, dir, "branch", "symlink-base")
	require.NoError(t, os.Remove(filepath.Join(dir, "link")))
	includeFile(t, dir, "link/hidden-name.env", "hidden-content", 0o600)
	includeFile(t, dir, "link/unprotected.env", "other", 0o600)
	includeFile(t, dir, "safe.env", "safe", 0o600)
	includeFile(t, dir, ".agentsignore", "link/hidden-name.env\n", 0o600)
	includeFile(t, dir, ".worktreeinclude", "*.env\n", 0o600)
	wt, err := Create(t.Context(), dir, "protected-symlink", WithRoot(t.TempDir()), WithBase("symlink-base"))
	require.NoError(t, err)
	require.Len(t, wt.Warnings, 2)
	joined := strings.Join(wt.Warnings, "\n")
	assert.Contains(t, joined, "checking destination ignore rules")
	assert.Contains(t, joined, "protected by .agentsignore")
	assert.NotContains(t, joined, "hidden-name")
	assert.NotContains(t, joined, "hidden-content")
	assert.FileExists(t, filepath.Join(wt.Dir, "safe.env"))
}

func TestWorktreeIncludeWarningBudget(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	includeFile(t, dir, ".gitignore", "protected/\n*.env\n", 0o644)
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "commit", "-m", "ignore local files")
	runGit(t, dir, "branch", "base")
	includeFile(t, dir, ".gitignore", "protected/\n*.env\n*.key\n", 0o644)
	includeFile(t, dir, ".worktreeinclude", "protected/\n*.key\n", 0o644)
	includeFile(t, dir, ".agentsignore", "protected/\n", 0o644)
	for i := range 150 {
		includeFile(t, dir, fmt.Sprintf("protected/hidden-%03d.env", i), "secret", 0o600)
		includeFile(t, dir, fmt.Sprintf("exposed-%03d.key", i), "secret", 0o600)
	}
	wt, err := Create(t.Context(), dir, "warnings", WithRoot(t.TempDir()), WithBase("base"))
	require.NoError(t, err)
	require.LessOrEqual(t, len(wt.Warnings), maxIncludeWarnings)
	joined := strings.Join(wt.Warnings, "\n")
	assert.LessOrEqual(t, len(joined), maxIncludeWarnings*(maxIncludeWarningBytes+1))
	assert.Contains(t, joined, "skipped 150 files protected by .agentsignore")
	assert.Contains(t, joined, "133 additional warnings omitted")
	assert.NotContains(t, joined, "hidden-")
}

type cancelAfterCopyStarts struct {
	context.Context //nolint:containedctx // Preserve cancellation while triggering it after the first write.

	path   string
	cancel context.CancelFunc
	size   int64
}

func (c *cancelAfterCopyStarts) Err() error {
	if info, err := os.Stat(c.path); err == nil && info.Size() > 0 {
		c.size = info.Size()
		c.cancel()
	}
	return c.Context.Err()
}

func TestWorktreeIncludeCancellationRemovesPartialFile(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	includeFile(t, dir, ".gitignore", "*.env\n", 0o644)
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "commit", "-m", "ignore env files")
	includeFile(t, dir, ".worktreeinclude", "*.env\n", 0o644)
	includeFile(t, dir, "a.env", strings.Repeat("x", 512*1024), 0o600)
	includeFile(t, dir, "b.env", "later", 0o600)
	root := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	trigger := &cancelAfterCopyStarts{Context: ctx, cancel: cancel, path: filepath.Join(root, "worktrees", "cancelled", "a.env")}
	wt, err := Create(trigger, dir, "cancelled", WithRoot(root))
	require.NoError(t, err)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	assert.Positive(t, trigger.size)
	assert.Less(t, trigger.size, int64(512*1024))
	assert.DirExists(t, wt.Dir)
	assert.NoFileExists(t, filepath.Join(wt.Dir, "a.env"))
	assert.NoFileExists(t, filepath.Join(wt.Dir, "b.env"))
	assert.Equal(t, []string{".worktreeinclude: copying cancelled: context canceled"}, wt.Warnings)
}

func TestIncludeWarningsTruncateUTF8(t *testing.T) {
	var warnings includeWarnings
	warnings.add(strings.Repeat("界", 300))
	require.Len(t, warnings.result(), 1)
	assert.LessOrEqual(t, len(warnings.entries[0]), maxIncludeWarningBytes)
	assert.True(t, utf8.ValidString(warnings.entries[0]))
}

func TestWorktreeIncludePrivateSourceAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	t.Parallel()
	base := t.TempDir()
	require.NoError(t, os.Chmod(base, 0o755))
	private := filepath.Join(base, "private")
	public := filepath.Join(base, "public")
	require.NoError(t, os.Mkdir(private, 0o700))
	require.NoError(t, os.Mkdir(public, 0o755))
	dir := filepath.Join(private, "repo")
	require.NoError(t, os.Mkdir(dir, 0o755))
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	includeFile(t, dir, ".gitignore", "*.env\n", 0o644)
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "commit", "-m", "base")
	includeFile(t, dir, ".worktreeinclude", "*.env\n", 0o644)
	includeFile(t, dir, "secret.env", "secret", 0o644)
	wt, err := Create(t.Context(), dir, "ancestor", WithRoot(public))
	require.NoError(t, err)
	info, err := os.Stat(filepath.Join(wt.Dir, "secret.env"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0), info.Mode().Perm()&0o077)
}

type cancelBetweenCopies struct {
	context.Context //nolint:containedctx // Observe the next loop check without cancelling the completed copy.

	path   string
	checks int
	cancel context.CancelFunc
}

func (c *cancelBetweenCopies) Err() error {
	if info, err := os.Stat(c.path); err == nil && info.Size() > 0 {
		c.checks++
		if c.checks == 3 {
			c.cancel()
		}
	}
	return c.Context.Err()
}

func TestWorktreeIncludeCancellationBetweenFiles(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	includeFile(t, dir, ".gitignore", "*.env\n", 0o644)
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "commit", "-m", "ignore env files")
	includeFile(t, dir, ".gitignore", "*.env\n*.key\n", 0o644)
	includeFile(t, dir, ".worktreeinclude", "*.env\n*.key\n", 0o644)
	includeFile(t, dir, "a.env", "first", 0o600)
	includeFile(t, dir, "z.key", "second", 0o600)
	root := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	trigger := &cancelBetweenCopies{Context: ctx, cancel: cancel, path: filepath.Join(root, "worktrees", "between", "a.env")}
	wt, err := Create(trigger, dir, "between", WithRoot(root))
	require.NoError(t, err)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	assert.FileExists(t, filepath.Join(wt.Dir, "a.env"))
	assert.NoFileExists(t, filepath.Join(wt.Dir, "z.key"))
	assert.Equal(t, []string{".worktreeinclude: copying cancelled: context canceled"}, wt.Warnings)
}

func TestWorktreeIncludeCancelledWithoutConfig(t *testing.T) {
	t.Parallel()
	dir := bootstrapRepo(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.Empty(t, copyIncludedFiles(ctx, dir, dir, t.TempDir()))
}

func TestWorktreeIncludePrivateDirectoryWithPublicAncestors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	t.Parallel()
	// A 0700 test temp directory would mask the in-repository restriction.
	base, err := os.MkdirTemp("/tmp", "worktree-include-public-") //nolint:forbidigo,usetesting // t.TempDir has private ancestors that hide permission regressions.
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, os.RemoveAll(base)) })
	require.NoError(t, os.Chmod(base, 0o755))
	dir := filepath.Join(base, "repo")
	require.NoError(t, os.Mkdir(dir, 0o755))
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	includeFile(t, dir, ".gitignore", "*.env\n", 0o644)
	includeFile(t, dir, "private/tracked", "tracked", 0o644)
	runGit(t, dir, "add", ".gitignore", "private/tracked")
	runGit(t, dir, "commit", "-m", "base")
	includeFile(t, dir, ".worktreeinclude", "*.env\n", 0o644)
	includeFile(t, dir, "private/secret.env", "secret", 0o644)
	require.NoError(t, os.Chmod(filepath.Join(dir, "private"), 0o700))
	wt, err := Create(t.Context(), dir, "inside", WithRoot(base))
	require.NoError(t, err)
	info, err := os.Stat(filepath.Join(wt.Dir, "private", "secret.env"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0), info.Mode().Perm()&0o077)
}

func TestWorktreeIncludePrivateRepositoryRootWithPublicAncestors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	t.Parallel()
	base, err := os.MkdirTemp("/tmp", "worktree-include-root-") //nolint:forbidigo,usetesting // Private temp ancestors would mask the repository-root restriction.
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, os.RemoveAll(base)) })
	require.NoError(t, os.Chmod(base, 0o755))
	dir := filepath.Join(base, "repo")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.Chmod(dir, 0o700))
	publicAncestors := func(path string) bool {
		resolved, err := filepath.EvalSymlinks(path)
		require.NoError(t, err)
		for parent := filepath.Dir(resolved); ; parent = filepath.Dir(parent) {
			info, err := os.Stat(parent)
			require.NoError(t, err)
			if info.Mode().Perm()&0o011 != 0o011 {
				return false
			}
			if filepath.Dir(parent) == parent {
				return true
			}
		}
	}
	if !publicAncestors(dir) {
		t.Skip("repository ancestors must be traversable by group and others")
	}
	rootInfo, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), rootInfo.Mode().Perm())
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	includeFile(t, dir, ".gitignore", "*.env\n", 0o644)
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "commit", "-m", "base")
	includeFile(t, dir, ".worktreeinclude", "*.env\n", 0o644)
	source := filepath.Join(dir, "secret.env")
	includeFile(t, dir, "secret.env", "secret", 0o644)
	require.NoError(t, os.Chmod(source, 0o644))
	wt, err := Create(t.Context(), dir, "private-root", WithRoot(base))
	require.NoError(t, err)
	assert.Empty(t, wt.Warnings)
	require.NoError(t, os.Chmod(filepath.Join(base, "worktrees"), 0o755))
	require.NoError(t, os.Chmod(wt.Dir, 0o755))
	dest := filepath.Join(wt.Dir, "secret.env")
	require.True(t, publicAncestors(dest), "destination ancestors must be traversable by group and others")
	info, err := os.Stat(dest)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	contents, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "secret", string(contents))
}
