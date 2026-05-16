package filesystem

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetHomeDir overrides $HOME for the duration of the test (and also
// $USERPROFILE on Windows, which os.UserHomeDir falls back to). The original
// values are restored when the test ends.
func resetHomeDir(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

func TestFilesystemTool_DefaultIsUnrestricted(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	// No allow_list, no deny_list: everything resolvable goes through.
	resolved, err := tool.resolveAndCheckPath("/etc/hosts")
	require.NoError(t, err)
	assert.Equal(t, "/etc/hosts", resolved)

	resolved, err = tool.resolveAndCheckPath("../../some/escape")
	require.NoError(t, err)
	// Equivalent to filepath.Clean of the joined relative escape.
	want := filepath.Clean(filepath.Join(tmpDir, "..", "..", "some", "escape"))
	assert.Equal(t, want, resolved)
}

func TestFilesystemTool_AllowList_DotMeansWorkingDir(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir, WithAllowList([]string{"."}))

	// Inside working dir is fine.
	_, err := tool.resolveAndCheckPath("file.txt")
	require.NoError(t, err)

	_, err = tool.resolveAndCheckPath("subdir/nested/file.txt")
	require.NoError(t, err)

	// Outside working dir is rejected.
	_, err = tool.resolveAndCheckPath("/etc/hosts")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside the allowed directories")

	// `..` traversals that escape the working dir are rejected.
	_, err = tool.resolveAndCheckPath("../escape.txt")
	require.Error(t, err)
}

func TestFilesystemTool_AllowList_TildeMeansHome(t *testing.T) {
	homeDir := t.TempDir()
	resetHomeDir(t, homeDir)
	wd := t.TempDir()

	tool := NewFilesystemTool(wd, WithAllowList([]string{"~"}))

	// A path under $HOME is allowed via ~/...
	resolved, err := tool.resolveAndCheckPath(filepath.Join(homeDir, "doc.md"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(homeDir, "doc.md"), resolved)

	// Working directory is NOT allowed (only ~ was listed).
	_, err = tool.resolveAndCheckPath("file.txt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside the allowed directories")
}

func TestFilesystemTool_AllowList_TildeSubdirectory(t *testing.T) {
	homeDir := t.TempDir()
	resetHomeDir(t, homeDir)
	require.NoError(t, os.MkdirAll(filepath.Join(homeDir, "projects"), 0o755))
	wd := t.TempDir()

	tool := NewFilesystemTool(wd, WithAllowList([]string{"~/projects"}))

	// Inside the listed subdir.
	_, err := tool.resolveAndCheckPath(filepath.Join(homeDir, "projects", "app", "main.go"))
	require.NoError(t, err)

	// $HOME itself is NOT inside ~/projects.
	_, err = tool.resolveAndCheckPath(filepath.Join(homeDir, "doc.md"))
	require.Error(t, err)

	// Sibling directory is rejected.
	_, err = tool.resolveAndCheckPath(filepath.Join(homeDir, "documents", "doc.md"))
	require.Error(t, err)
}

func TestFilesystemTool_AllowList_MultipleRoots(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	otherDir := t.TempDir()

	tool := NewFilesystemTool(wd, WithAllowList([]string{".", otherDir}))

	_, err := tool.resolveAndCheckPath("file.txt")
	require.NoError(t, err)

	_, err = tool.resolveAndCheckPath(filepath.Join(otherDir, "file.txt"))
	require.NoError(t, err)

	_, err = tool.resolveAndCheckPath("/etc/hosts")
	require.Error(t, err)
}

func TestFilesystemTool_AllowList_AbsolutePath(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	allowed := t.TempDir()

	tool := NewFilesystemTool(wd, WithAllowList([]string{allowed}))

	// Absolute path inside the allowed root is fine.
	_, err := tool.resolveAndCheckPath(filepath.Join(allowed, "x", "y.txt"))
	require.NoError(t, err)

	// Absolute path outside is rejected.
	_, err = tool.resolveAndCheckPath("/etc/hosts")
	require.Error(t, err)
}

func TestFilesystemTool_DenyList_RejectsMatchingPaths(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	denied := filepath.Join(wd, "secret")
	require.NoError(t, os.Mkdir(denied, 0o755))

	tool := NewFilesystemTool(wd, WithDenyList([]string{"secret"}))

	// Anything under the denied subtree is rejected.
	_, err := tool.resolveAndCheckPath("secret/key.pem")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "denied directory")

	// Sibling files are still reachable.
	_, err = tool.resolveAndCheckPath("public.md")
	require.NoError(t, err)

	// And — because no allow-list is set — paths outside the working dir
	// are still allowed (deny-only configurations preserve broad access).
	_, err = tool.resolveAndCheckPath("/etc/hosts")
	require.NoError(t, err)
}

func TestFilesystemTool_DenyList_TakesPrecedenceOverAllowList(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(wd, "src"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(wd, "src", "vendor"), 0o755))

	tool := NewFilesystemTool(wd,
		WithAllowList([]string{"."}),
		WithDenyList([]string{"src/vendor"}))

	// Allowed by allow-list, not denied.
	_, err := tool.resolveAndCheckPath("src/main.go")
	require.NoError(t, err)

	// Denied even though it's inside the allow-list.
	_, err = tool.resolveAndCheckPath("src/vendor/lib.go")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "denied directory")
}

func TestFilesystemTool_AllowList_SymlinkEscapeRejected(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	target := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(target, "secret.txt"), []byte("nope"), 0o644))

	// Plant a symlink inside the working dir that points outside.
	link := filepath.Join(wd, "escape")
	require.NoError(t, os.Symlink(target, link))

	tool := NewFilesystemTool(wd, WithAllowList([]string{"."}))

	// Following the symlink escapes the allow-list and must be rejected.
	_, err := tool.resolveAndCheckPath("escape/secret.txt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside the allowed directories")
}

func TestFilesystemTool_DenyList_SymlinkIntoDeniedAreaRejected(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	denied := filepath.Join(wd, "secret")
	require.NoError(t, os.Mkdir(denied, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(denied, "key.pem"), []byte("nope"), 0o644))

	// Symlink that lives outside the denied directory but points into it.
	link := filepath.Join(wd, "shortcut")
	require.NoError(t, os.Symlink(denied, link))

	tool := NewFilesystemTool(wd, WithDenyList([]string{"secret"}))

	// Reading via the symlink must still trigger the deny-list.
	_, err := tool.resolveAndCheckPath("shortcut/key.pem")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "denied directory")
}

func TestFilesystemTool_AllowList_NewFilePath(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	tool := NewFilesystemTool(wd, WithAllowList([]string{"."}))

	// A path that doesn't exist yet (e.g. about to be created by write_file)
	// must still be accepted when its lexical location is inside the allow-list.
	_, err := tool.resolveAndCheckPath("new/dir/output.txt")
	require.NoError(t, err)

	// But if the new path's parent escapes the allow-list (via ..) it's
	// rejected.
	_, err = tool.resolveAndCheckPath("../new.txt")
	require.Error(t, err)
}

func TestFilesystemTool_AllowList_EmptyDisablesCheck(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// nil and empty slice both leave the allow-list disabled.
	for _, roots := range [][]string{nil, {}} {
		tool := NewFilesystemTool(tmpDir, WithAllowList(roots))
		_, err := tool.resolveAndCheckPath("/etc/hosts")
		require.NoError(t, err, "empty/nil allow-list must not constrain")
	}
}

func TestFilesystemTool_HandlersUseAllowList(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	other := t.TempDir()

	// Pre-populate a file outside the working dir.
	outsideFile := filepath.Join(other, "outside.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("nope"), 0o644))

	tool := NewFilesystemTool(wd, WithAllowList([]string{"."}))

	// read_file: must refuse the outside path.
	res, err := tool.handleReadFile(t.Context(), ReadFileArgs{Path: outsideFile})
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Output, "outside the allowed directories")

	// write_file: must refuse to write outside, and must NOT create the file.
	res, err = tool.handleWriteFile(t.Context(), WriteFileArgs{
		Path:    filepath.Join(other, "should-not-exist.txt"),
		Content: "x",
	})
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.NoFileExists(t, filepath.Join(other, "should-not-exist.txt"))

	// list_directory: must refuse the outside path.
	res, err = tool.handleListDirectory(t.Context(), ListDirectoryArgs{Path: other})
	require.NoError(t, err)
	assert.True(t, res.IsError)

	// search_files_content: must refuse the outside path.
	res, err = tool.handleSearchFilesContent(t.Context(), SearchFilesContentArgs{
		Path:  other,
		Query: "nope",
	})
	require.NoError(t, err)
	assert.True(t, res.IsError)

	// directory_tree: must refuse the outside path.
	res, err = tool.handleDirectoryTree(t.Context(), DirectoryTreeArgs{Path: other})
	require.NoError(t, err)
	assert.True(t, res.IsError)

	// create_directory: must refuse, and must not create the directory.
	res, err = tool.handleCreateDirectory(t.Context(), CreateDirectoryArgs{
		Paths: []string{filepath.Join(other, "newdir")},
	})
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.NoDirExists(t, filepath.Join(other, "newdir"))

	// remove_directory: must refuse to operate on the outside path.
	require.NoError(t, os.Mkdir(filepath.Join(other, "keep"), 0o755))
	res, err = tool.handleRemoveDirectory(t.Context(), RemoveDirectoryArgs{
		Paths: []string{filepath.Join(other, "keep")},
	})
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.DirExists(t, filepath.Join(other, "keep"))

	// read_multiple_files: per-path errors don't fail the whole call but
	// each rejected path is reported in the output.
	require.NoError(t, os.WriteFile(filepath.Join(wd, "ok.txt"), []byte("ok"), 0o644))
	res, err = tool.handleReadMultipleFiles(t.Context(), ReadMultipleFilesArgs{
		Paths: []string{"ok.txt", outsideFile},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "ok") // the legal one was read
	assert.Contains(t, res.Output, "outside the allowed directories")
}

func TestFilesystemTool_HandlersUseDenyList(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(wd, "secrets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wd, "secrets", "key.pem"), []byte("k"), 0o644))

	tool := NewFilesystemTool(wd, WithDenyList([]string{"secrets"}))

	// edit_file: must refuse to read the file in a denied directory.
	res, err := tool.handleEditFile(t.Context(), EditFileArgs{
		Path:  "secrets/key.pem",
		Edits: []Edit{{OldText: "k", NewText: "tampered"}},
	})
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Output, "denied directory")
	// The file content must not have been modified.
	got, err := os.ReadFile(filepath.Join(wd, "secrets", "key.pem"))
	require.NoError(t, err)
	assert.Equal(t, "k", string(got))
}

func TestFilesystemTool_Instructions_MentionsRestrictions(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()

	// Default instructions: no restriction text.
	plain := NewFilesystemTool(wd).Instructions()
	assert.NotContains(t, plain, "restricted")
	assert.NotContains(t, plain, "must not access")

	// With an allow-list: instructions mention the restriction.
	allowed := NewFilesystemTool(wd, WithAllowList([]string{".", "~"})).Instructions()
	assert.Contains(t, allowed, "restricted")
	assert.Contains(t, allowed, ".")
	assert.Contains(t, allowed, "~")

	// With a deny-list: instructions mention the deny entries.
	denied := NewFilesystemTool(wd, WithDenyList([]string{"~/.ssh"})).Instructions()
	assert.Contains(t, denied, "must not access")
	assert.Contains(t, denied, "~/.ssh")
}

func TestExpandPathToken(t *testing.T) {
	homeDir := t.TempDir()
	resetHomeDir(t, homeDir)
	wd := t.TempDir()
	t.Setenv("MY_VAR", "/var/data")
	t.Setenv("EMPTY_VAR", "")
	os.Unsetenv("DEFINITELY_NOT_SET")

	tests := []struct {
		name    string
		token   string
		want    string
		wantErr string // substring match; empty = no error
	}{
		{name: "dot", token: ".", want: wd},
		{name: "tilde", token: "~", want: homeDir},
		{name: "tilde-subdir", token: "~/projects", want: filepath.Join(homeDir, "projects")},
		{name: "absolute", token: "/srv/data", want: "/srv/data"},
		{name: "relative", token: "src", want: filepath.Join(wd, "src")},
		{name: "env-var", token: "$MY_VAR", want: "/var/data"},
		{name: "env-var-braces", token: "${MY_VAR}", want: "/var/data"},
		{name: "env-var-inside-tilde", token: "~/${MY_VAR}", want: filepath.Join(homeDir, "var", "data")},
		{name: "empty", token: "", wantErr: "empty"},
		{name: "whitespace", token: "   ", wantErr: "empty"},
		// Regression: an undefined env var must NOT silently expand to the
		// working directory — a typo in the var name would otherwise grant
		// (or close) access to the entire working dir.
		{name: "undefined-env-var", token: "$DEFINITELY_NOT_SET", wantErr: "empty string"},
		{name: "defined-but-empty-env-var", token: "$EMPTY_VAR", wantErr: "empty string"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expandPathToken(wd, tc.token)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestWithAllowList_RejectsUndefinedEnvVar(t *testing.T) {
	// Regression test: a typo in an env-var name in allow_list must NOT
	// silently grant access to the working directory. The toolset must
	// fail-closed: reject all operations when list construction fails.
	os.Unsetenv("DEFINITELY_NOT_SET")
	wd := t.TempDir()
	tool := NewFilesystemTool(wd, WithAllowList([]string{"$DEFINITELY_NOT_SET"}))

	// The allow-list construction failed, so the toolset is disabled
	// (fail-closed). All operations must be rejected.
	_, err := tool.resolveAndCheckPath("/etc/hosts")
	require.Error(t, err, "undefined env var must cause toolset to fail-closed")
	assert.Contains(t, err.Error(), "disabled due to invalid")

	// Also verify working dir access is rejected (not silently allowed).
	_, err = tool.resolveAndCheckPath("file.txt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disabled due to invalid")
}

func TestWithAllowList_AcceptsDefinedEnvVar(t *testing.T) {
	wd := t.TempDir()
	allowed := t.TempDir()
	t.Setenv("ALLOWED_DIR", allowed)

	tool := NewFilesystemTool(wd, WithAllowList([]string{"$ALLOWED_DIR"}))

	// Inside the env-var-resolved root.
	_, err := tool.resolveAndCheckPath(filepath.Join(allowed, "file.txt"))
	require.NoError(t, err)

	// Outside is rejected — confirms the allow-list is actually active.
	_, err = tool.resolveAndCheckPath("/etc/hosts")
	require.Error(t, err)
}

func TestDenyList_NonExistentPath(t *testing.T) {
	// A common usage: deny ~/.ssh on a system that does not have a ~/.ssh
	// yet. The deny-list must still apply when the directory is created
	// after the toolset is constructed.
	homeDir := t.TempDir()
	resetHomeDir(t, homeDir)
	wd := t.TempDir()

	tool := NewFilesystemTool(wd, WithDenyList([]string{"~/.ssh"}))

	// ~/.ssh does not exist yet — a write to a path inside it must be
	// rejected before the directory is even created.
	_, err := tool.resolveAndCheckPath(filepath.Join(homeDir, ".ssh", "id_rsa"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "denied directory")

	// Sibling files in $HOME are still reachable.
	_, err = tool.resolveAndCheckPath(filepath.Join(homeDir, "doc.md"))
	require.NoError(t, err)

	// Now create ~/.ssh and re-test — still denied.
	require.NoError(t, os.Mkdir(filepath.Join(homeDir, ".ssh"), 0o700))
	_, err = tool.resolveAndCheckPath(filepath.Join(homeDir, ".ssh", "id_rsa"))
	require.Error(t, err)
}

func TestPathRootSet_DeduplicatesEntries(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()

	set, err := newPathRootSet(wd, []string{".", ".", wd, "subdir/.."})
	require.NoError(t, err)
	require.NotNil(t, set)
	// All four resolve to wd.
	assert.Len(t, set.entries, 1)
	// Only one *os.Root must be retained — duplicates' handles must be
	// closed during construction to avoid an fd leak.
	require.NotNil(t, set.entries[0].root)
	set.close()
}

func TestPathRootSet_InvalidEntryClosesEarlierRoots(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()

	// First two entries are valid (they open *os.Root handles); the third
	// is empty and triggers an error. The constructor must release the
	// handles it opened so far rather than leaking them.
	set, err := newPathRootSet(wd, []string{".", wd, ""})
	require.Error(t, err)
	assert.Nil(t, set)
}

func TestPathRootSet_NilForEmptyInput(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()

	set, err := newPathRootSet(wd, nil)
	require.NoError(t, err)
	assert.Nil(t, set)

	set, err = newPathRootSet(wd, []string{})
	require.NoError(t, err)
	assert.Nil(t, set)
}

func TestPathRootSet_RejectsEmptyEntry(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()

	_, err := newPathRootSet(wd, []string{".", ""})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestPathRootSet_OpensOSRootForSandboxing(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(wd, "ok"), 0o755))

	set, err := newPathRootSet(wd, []string{"."})
	require.NoError(t, err)
	require.NotNil(t, set)
	require.Len(t, set.entries, 1)
	// Bonus: the entry should hold an *os.Root for the existing directory,
	// which is what gives us TOCTOU-safe containment checks.
	assert.NotNil(t, set.entries[0].root, "expected an *os.Root for an existing directory")

	set.close()
}
