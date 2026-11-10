package worktree

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

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
		assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
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
	assert.Len(t, wt.Warnings, 2)
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
		t.Run(path, func(t *testing.T) { require.Error(t, copyIncludedFile(src, dst, path)) })
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
	includeFile(t, dir, ".worktreeinclude", "*.env\n", 0o644)
	wt, err := Create(t.Context(), dir, "symlink-dest", WithRoot(t.TempDir()), WithBase("destination-symlink"))
	require.NoError(t, err)
	require.Len(t, wt.Warnings, 1)
	assert.Contains(t, wt.Warnings[0], "checking destination ignore rules")
	assert.FileExists(t, filepath.Join(wt.Dir, "safe.env"))
	assert.Empty(t, gitOut(t, wt.Dir, "status", "--porcelain"))
	runGit(t, wt.Dir, "add", "-A")
	assert.Empty(t, gitOut(t, wt.Dir, "diff", "--cached", "--name-only"))
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
