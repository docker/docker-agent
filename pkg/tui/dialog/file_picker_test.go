package dialog

import (
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestFilePickerDialog creates a filePickerDialog pointed at dir without
// going through NewFilePickerDialog (which uses os.Getwd).
func newTestFilePickerDialog(dir string) *filePickerDialog {
	d := &filePickerDialog{
		pickerCore: newPickerCore(filePickerLayout, "Type to filter files…"),
		currentDir: dir,
	}
	d.loadDirectory()
	return d
}

// setupTestDir creates a temporary directory tree for file-picker tests:
//
//	tmpdir/
//	  visible_dir/
//	  .hidden_dir/
//	  visible_file.txt
//	  .hidden_file
func setupTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	require.NoError(t, os.Mkdir(filepath.Join(dir, "visible_dir"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".hidden_dir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "visible_file.txt"), []byte("hi"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".hidden_file"), []byte("secret"), 0o644))

	return dir
}

// entryNames returns the names from a slice of fileEntry.
func entryNames(entries []fileEntry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.name
	}
	return names
}

func TestFilePickerHiddenFilesFilteredByDefault(t *testing.T) {
	t.Parallel()
	dir := setupTestDir(t)

	d := newTestFilePickerDialog(dir)

	names := entryNames(d.entries)
	assert.Contains(t, names, "visible_dir/")
	assert.Contains(t, names, "visible_file.txt")
	assert.NotContains(t, names, ".hidden_dir/")
	assert.NotContains(t, names, ".hidden_file")
}

func TestFilePickerShowHiddenFiles(t *testing.T) {
	t.Parallel()
	dir := setupTestDir(t)

	d := newTestFilePickerDialog(dir)
	d.showHidden = true
	d.loadDirectory()

	names := entryNames(d.entries)
	assert.Contains(t, names, "visible_dir/")
	assert.Contains(t, names, "visible_file.txt")
	assert.Contains(t, names, ".hidden_dir/")
	assert.Contains(t, names, ".hidden_file")
}

func TestFilePickerToggleHiddenViaAltH(t *testing.T) {
	t.Parallel()
	dir := setupTestDir(t)

	d := newTestFilePickerDialog(dir)
	d.SetSize(100, 50)

	// Initially hidden files are filtered out.
	require.False(t, d.showHidden)
	names := entryNames(d.filtered)
	require.NotContains(t, names, ".hidden_file")

	// Press alt+h to toggle hidden files on.
	altH := tea.KeyPressMsg{Code: 'h', Mod: tea.ModAlt}
	updated, _ := d.Update(altH)
	d = updated.(*filePickerDialog)

	require.True(t, d.showHidden)
	names = entryNames(d.filtered)
	assert.Contains(t, names, ".hidden_dir/")
	assert.Contains(t, names, ".hidden_file")

	// Press alt+h again to toggle hidden files off.
	updated, _ = d.Update(altH)
	d = updated.(*filePickerDialog)

	require.False(t, d.showHidden)
	names = entryNames(d.filtered)
	assert.NotContains(t, names, ".hidden_dir/")
	assert.NotContains(t, names, ".hidden_file")
}

func TestFilePickerToggleIgnoredViaAltI(t *testing.T) {
	t.Parallel()
	dir := setupTestDir(t)

	d := newTestFilePickerDialog(dir)
	d.SetSize(100, 50)

	// Initially showIgnored is false.
	require.False(t, d.showIgnored)

	// Press alt+i to toggle.
	altI := tea.KeyPressMsg{Code: 'i', Mod: tea.ModAlt}
	updated, _ := d.Update(altI)
	d = updated.(*filePickerDialog)
	require.True(t, d.showIgnored)

	// Press alt+i again to toggle back.
	updated, _ = d.Update(altI)
	d = updated.(*filePickerDialog)
	require.False(t, d.showIgnored)
}

func TestFilePickerShowIgnoredInGitRepo(t *testing.T) {
	t.Parallel()

	// Set up a minimal git repo with a .gitignore that ignores *.log files.
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o755))
	// Minimal git structure: HEAD file pointing to a ref.
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(gitDir, "objects"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(gitDir, "refs"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\nbuild/\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hello"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "debug.log"), []byte("log data"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "build"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "src"), 0o755))

	// Default: ignored files/dirs should be filtered out.
	d := newTestFilePickerDialog(dir)
	// Enable showHidden so .gitignore itself is visible.
	d.showHidden = true
	d.loadDirectory()

	names := entryNames(d.entries)
	assert.Contains(t, names, "readme.txt")
	assert.Contains(t, names, "src/")
	assert.NotContains(t, names, "debug.log", "gitignored file should be hidden by default")
	assert.NotContains(t, names, "build/", "gitignored dir should be hidden by default")

	// Toggle showIgnored on.
	d.showIgnored = true
	d.loadDirectory()

	names = entryNames(d.entries)
	assert.Contains(t, names, "debug.log", "gitignored file should be visible when showIgnored=true")
	assert.Contains(t, names, "build/", "gitignored dir should be visible when showIgnored=true")
	assert.Contains(t, names, "readme.txt")
}

func TestFilePickerHelpKeysRows(t *testing.T) {
	t.Parallel()

	d := &filePickerDialog{}

	// Default state: both off.
	row1, row2 := d.filePickerHelpKeysRows()
	assert.Equal(t, []string{
		"↑/↓", "navigate",
		"enter", "select",
		"esc", "close",
		"alt+h", "show hidden",
	}, row1)
	assert.Equal(t, []string{
		"alt+i", "show ignored",
	}, row2)

	// showHidden on.
	d.showHidden = true
	row1, _ = d.filePickerHelpKeysRows()
	assert.Contains(t, row1, "hide hidden")

	// showIgnored on.
	d.showIgnored = true
	_, row2 = d.filePickerHelpKeysRows()
	assert.Contains(t, row2, "hide ignored")

	// Both off again.
	d.showHidden = false
	d.showIgnored = false
	row1, row2 = d.filePickerHelpKeysRows()
	assert.Contains(t, row1, "show hidden")
	assert.Contains(t, row2, "show ignored")
}

func TestFilePickerDirectoriesListedBeforeFiles(t *testing.T) {
	t.Parallel()
	dir := setupTestDir(t)

	d := newTestFilePickerDialog(dir)

	// The first entries (after "..") should be directories, then files.
	foundFile := false
	for _, e := range d.entries {
		if e.name == ".." {
			continue
		}
		if !e.isDir {
			foundFile = true
		}
		if foundFile && e.isDir {
			t.Errorf("directory %q listed after file entries", e.name)
		}
	}
}

func TestFilePickerParentDirEntry(t *testing.T) {
	t.Parallel()
	dir := setupTestDir(t)

	d := newTestFilePickerDialog(dir)

	require.NotEmpty(t, d.entries)
	require.Equal(t, "..", d.entries[0].name, "first entry should be parent dir")
	require.True(t, d.entries[0].isDir)
	require.Equal(t, filepath.Dir(dir), d.entries[0].path)
}

func TestFilePickerFilterPreservesParentDir(t *testing.T) {
	t.Parallel()
	dir := setupTestDir(t)

	d := newTestFilePickerDialog(dir)

	// Set a filter that doesn't match ".."
	d.textInput.SetValue("visible")
	d.filterEntries()

	// ".." should always be present in filtered results.
	names := entryNames(d.filtered)
	assert.Contains(t, names, "..", "parent dir entry should always appear in filtered results")
}

func TestFilePickerHiddenDirsAndFilesSeparately(t *testing.T) {
	t.Parallel()
	dir := setupTestDir(t)

	// With showHidden=false, neither hidden dirs nor hidden files should appear.
	d := newTestFilePickerDialog(dir)
	names := entryNames(d.entries)
	assert.NotContains(t, names, ".hidden_dir/")
	assert.NotContains(t, names, ".hidden_file")

	// With showHidden=true, both should appear.
	d.showHidden = true
	d.loadDirectory()
	names = entryNames(d.entries)
	assert.Contains(t, names, ".hidden_dir/")
	assert.Contains(t, names, ".hidden_file")
}
