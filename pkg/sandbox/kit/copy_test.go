package kit

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopyTree_Symlinks(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	src := filepath.Join(parent, "skill")
	require.NoError(t, os.Mkdir(src, 0o700))
	sibling := filepath.Join(parent, "skill-sibling")
	require.NoError(t, os.Mkdir(sibling, 0o700))
	outside := filepath.Join(sibling, "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(src, "helper.txt"), []byte("helper"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(src, "nested"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, "nested", "file.txt"), []byte("nested"), 0o600))
	for name, target := range map[string]string{
		"relative":    "helper.txt",
		"absolute":    filepath.Join(src, "helper.txt"),
		"chain":       "absolute",
		"outside":     outside,
		"escape":      filepath.Join("..", "skill-sibling", "outside.txt"),
		"outside-dir": sibling,
		"dangling":    "missing.txt",
		"dir":         "nested",
		"loop":        "loop",
	} {
		require.NoError(t, os.Symlink(target, filepath.Join(src, name)))
	}
	kitDir := t.TempDir()
	dst := filepath.Join(kitDir, "skill")
	_, err := copyTree(kitDir, src, dst)
	require.NoError(t, err)
	for _, name := range []string{"helper.txt", "relative", "absolute", "chain"} {
		data, err := os.ReadFile(filepath.Join(dst, name))
		require.NoError(t, err)
		assert.Equal(t, "helper", string(data))
	}
	for _, name := range []string{"outside", "escape", "outside-dir", "dangling", "dir", "loop"} {
		_, err := os.Lstat(filepath.Join(dst, name))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	data, err := os.ReadFile(filepath.Join(dst, "nested", "file.txt"))
	require.NoError(t, err)
	assert.Equal(t, "nested", string(data))
}

func TestCopyRootedFile_RejectsReplacedPath(t *testing.T) {
	t.Parallel()
	for _, ancestor := range []bool{false, true} {
		name := "file"
		if ancestor {
			name = "ancestor"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			src := t.TempDir()
			outside := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(src, "nested"), 0o700))
			filePath := filepath.Join(src, "nested", "file.txt")
			require.NoError(t, os.WriteFile(filePath, []byte("original"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(outside, "file.txt"), []byte("outside"), 0o600))
			root, err := os.OpenRoot(src)
			require.NoError(t, err)
			defer root.Close()
			// Capture the target before replacing it, as the walk or symlink resolver would.
			resolved, err := filepath.EvalSymlinks(filePath)
			require.NoError(t, err)
			canonical, err := filepath.EvalSymlinks(src)
			require.NoError(t, err)
			rel, err := filepath.Rel(canonical, resolved)
			require.NoError(t, err)
			if ancestor {
				require.NoError(t, os.RemoveAll(filepath.Dir(filePath)))
				require.NoError(t, os.Symlink(outside, filepath.Dir(filePath)))
			} else {
				require.NoError(t, os.Remove(filePath))
				require.NoError(t, os.Symlink(filepath.Join(outside, "file.txt"), filePath))
			}
			kitDir := t.TempDir()
			dst := filepath.Join(kitDir, "copied.txt")
			redaction, err := copyRootedFile(kitDir, root, rel, resolved, dst)
			require.Error(t, err)
			assert.Nil(t, redaction)
			assert.NoFileExists(t, dst)
		})
	}
}

func TestCopyRootedTree_KeepsOpenedRoot(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not allow renaming an open directory")
	}
	parent := t.TempDir()
	src := filepath.Join(parent, "skill")
	require.NoError(t, os.Mkdir(src, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, "file.txt"), []byte("original"), 0o600))
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "file.txt"), []byte("outside"), 0o600))
	root, err := os.OpenRoot(src)
	require.NoError(t, err)
	defer root.Close()
	require.NoError(t, os.Symlink("file.txt", filepath.Join(src, "alias.txt")))
	require.NoError(t, os.Rename(src, filepath.Join(parent, "moved")))
	require.NoError(t, os.Symlink(outside, src))
	kitDir := t.TempDir()
	dst := filepath.Join(kitDir, "skill")
	_, err = copyRootedTree(kitDir, root, src, dst)
	require.NoError(t, err)
	for _, name := range []string{"file.txt", "alias.txt"} {
		data, err := os.ReadFile(filepath.Join(dst, name))
		require.NoError(t, err)
		assert.Equal(t, "original", string(data))
	}
}

func TestCopyOpenedFile_UsesOpenedContentAndMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support Unix executable permission bits")
	}
	src := filepath.Join(t.TempDir(), "helper.sh")
	require.NoError(t, os.WriteFile(src, []byte("original"), 0o700))
	file, err := os.Open(src)
	require.NoError(t, err)
	defer file.Close()
	require.NoError(t, os.Remove(src))
	require.NoError(t, os.WriteFile(src, []byte("replacement"), 0o600))
	kitDir := t.TempDir()
	dst := filepath.Join(kitDir, "helper.sh")
	_, err = copyOpenedFile(kitDir, file, src, dst)
	require.NoError(t, err)
	data, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, "original", string(data))
	info, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestCopyTree_SkipsSymlinkedRoot(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "private.txt"), []byte("outside"), 0o600))
	src := filepath.Join(t.TempDir(), "skill")
	require.NoError(t, os.Symlink(outside, src))
	kitDir := t.TempDir()
	dst := filepath.Join(kitDir, "skill")
	redactions, err := copyTree(kitDir, src, dst)
	require.NoError(t, err)
	assert.Empty(t, redactions)
	assert.NoDirExists(t, dst)
}
