package workspacemedia

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var pngData = []byte("fake-png-bytes")

func readWorkspaceFile(t *testing.T, root, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	require.NoError(t, err)
	return data
}

func TestWrite_PlainName(t *testing.T) {
	root := t.TempDir()

	res, err := Write(root, "sunshine.png", pngData, "image/png")
	require.NoError(t, err)
	assert.Equal(t, "sunshine.png", res.RelPath)
	assert.False(t, res.ExtensionCorrected)
	assert.Equal(t, pngData, readWorkspaceFile(t, root, res.RelPath))
}

func TestWrite_SanitizesFilename(t *testing.T) {
	root := t.TempDir()

	res, err := Write(root, `my <cool>: "image?".png`, pngData, "image/png")
	require.NoError(t, err)
	assert.Equal(t, "my -cool-- -image--.png", res.RelPath)
	assert.Equal(t, pngData, readWorkspaceFile(t, root, res.RelPath))
}

func TestWrite_TrimsWindowsUnsafeTrailingChars(t *testing.T) {
	root := t.TempDir()

	// Windows silently strips trailing dots/spaces; the writer trims them
	// up front so the returned path is exactly what exists on disk.
	res, err := Write(root, "photo...png", pngData, "image/png")
	require.NoError(t, err)
	assert.Equal(t, "photo.png", res.RelPath)
}

func TestWrite_CorrectsConflictingExtension(t *testing.T) {
	root := t.TempDir()

	res, err := Write(root, "sunshine.jpg", pngData, "image/png")
	require.NoError(t, err)
	assert.Equal(t, "sunshine.png", res.RelPath)
	assert.True(t, res.ExtensionCorrected)
	assert.Equal(t, ".jpg", res.RequestedExtension)
	assert.Equal(t, pngData, readWorkspaceFile(t, root, res.RelPath))
}

func TestWrite_KeepsMatchingExtensionVariant(t *testing.T) {
	root := t.TempDir()

	for _, name := range []string{"a.jpeg", "b.JPG"} {
		res, err := Write(root, name, []byte("jpeg"), "image/jpeg")
		require.NoError(t, err)
		assert.Equal(t, name, res.RelPath)
		assert.False(t, res.ExtensionCorrected)
	}
}

func TestWrite_NormalizesMIMEParameters(t *testing.T) {
	root := t.TempDir()

	res, err := Write(root, "pic", pngData, "image/png; some=param")
	require.NoError(t, err)
	assert.Equal(t, "pic.png", res.RelPath)
}

func TestWrite_ExtensionlessName(t *testing.T) {
	root := t.TempDir()

	res, err := Write(root, "sunshine", pngData, "image/png")
	require.NoError(t, err)
	assert.Equal(t, "sunshine.png", res.RelPath)
	assert.False(t, res.ExtensionCorrected)
}

func TestWrite_UnknownMIME(t *testing.T) {
	root := t.TempDir()

	res, err := Write(root, "blob", []byte("data"), "")
	require.NoError(t, err)
	assert.Equal(t, "blob.bin", res.RelPath)
	assert.False(t, res.ExtensionCorrected)

	// An unknown MIME type cannot contradict a requested extension: keep it.
	res, err = Write(root, "notes.dat", []byte("data"), "application/x-mystery")
	require.NoError(t, err)
	assert.Equal(t, "notes.dat", res.RelPath)
	assert.False(t, res.ExtensionCorrected)
}

func TestWrite_GenericFallbackName(t *testing.T) {
	root := t.TempDir()

	for i, want := range []string{"generated.png", "generated-1.png", "generated-2.png"} {
		res, err := Write(root, "generated", pngData, "image/png")
		require.NoError(t, err, "write %d", i)
		assert.Equal(t, want, res.RelPath)
	}
}

func TestWrite_NestedSubdirsCreated(t *testing.T) {
	root := t.TempDir()

	res, err := Write(root, "images/out/pic.png", pngData, "image/png")
	require.NoError(t, err)
	assert.Equal(t, "images/out/pic.png", res.RelPath)
	assert.Equal(t, pngData, readWorkspaceFile(t, root, res.RelPath))

	if runtime.GOOS != "windows" { // modes are ACL-inherited on Windows
		dirInfo, err := os.Stat(filepath.Join(root, "images", "out"))
		require.NoError(t, err)
		assert.Zero(t, dirInfo.Mode().Perm()&^0o755, "dir mode %v exceeds 0755", dirInfo.Mode().Perm())

		fileInfo, err := os.Stat(filepath.Join(root, "images", "out", "pic.png"))
		require.NoError(t, err)
		assert.Zero(t, fileInfo.Mode().Perm()&^0o644, "file mode %v exceeds 0644", fileInfo.Mode().Perm())
	}
}

func TestWrite_CurrentDirSegmentsIgnored(t *testing.T) {
	root := t.TempDir()

	res, err := Write(root, "./images/./pic.png", pngData, "image/png")
	require.NoError(t, err)
	assert.Equal(t, "images/pic.png", res.RelPath)
}

func TestWrite_RejectsEscapingAndInvalidPaths(t *testing.T) {
	root := t.TempDir()

	for _, requested := range []string{
		"/etc/passwd",
		`\evil.png`,
		`C:\evil.png`,
		"c:/evil.png",
		"..",
		"../pic.png",
		`..\pic.png`,
		"a/../pic.png",
		"a/../../pic.png",
		"",
		".",
		"...",
		"   ",
		"a/.../b.png",
		"CON",
		"con.png",
		"images/NUL/pic.png",
		"lpt1.txt",
	} {
		t.Run(fmt.Sprintf("%q", requested), func(t *testing.T) {
			_, err := Write(root, requested, pngData, "image/png")
			require.ErrorIs(t, err, ErrPathEscape)
		})
	}

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, entries, "rejected paths must not leave files behind")
}

func TestWrite_RejectsSymlinkedDirEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "link")))

	for _, requested := range []string{"link/pic.png", "link/sub/pic.png"} {
		_, err := Write(root, requested, pngData, "image/png")
		require.ErrorIs(t, err, ErrPathEscape, "requested %q", requested)
	}

	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing may be written through the symlink")
}

func TestWrite_NeverWritesThroughSymlinkAtLeaf(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "target.png")
	require.NoError(t, os.Symlink(target, filepath.Join(root, "pic.png")))

	// O_EXCL treats the existing symlink as "name taken": the write lands
	// on the dash-suffixed sibling and the symlink target is never created.
	res, err := Write(root, "pic.png", pngData, "image/png")
	require.NoError(t, err)
	assert.Equal(t, "pic-1.png", res.RelPath)
	assert.NoFileExists(t, target)
}

func TestWrite_CollisionGetsDashSuffix(t *testing.T) {
	root := t.TempDir()
	existing := []byte("do-not-touch")
	require.NoError(t, os.WriteFile(filepath.Join(root, "sunshine.png"), existing, 0o644))

	res, err := Write(root, "sunshine.png", pngData, "image/png")
	require.NoError(t, err)
	assert.Equal(t, "sunshine-1.png", res.RelPath)
	assert.Equal(t, existing, readWorkspaceFile(t, root, "sunshine.png"))
	assert.Equal(t, pngData, readWorkspaceFile(t, root, "sunshine-1.png"))
}

type destinationCreatingReader struct {
	target  string
	created []byte
	data    []byte
	done    bool
}

func (r *destinationCreatingReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	if err := os.WriteFile(r.target, r.created, 0o644); err != nil {
		return 0, err
	}
	r.done = true
	return copy(p, r.data), nil
}

func TestWrite_DestinationCreatedDuringCopyIsPreserved(t *testing.T) {
	root := t.TempDir()
	created := []byte("created-during-copy")
	generated := []byte("generated-content")
	r := &destinationCreatingReader{
		target:  filepath.Join(root, "pic.png"),
		created: created,
		data:    generated,
	}

	res, err := write(root, "pic.png", r, "image/png")
	require.NoError(t, err)
	assert.Equal(t, "pic-1.png", res.RelPath)
	assert.Equal(t, created, readWorkspaceFile(t, root, "pic.png"))
	assert.Equal(t, generated, readWorkspaceFile(t, root, "pic-1.png"))

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 2, "publication must not leak its sibling temp file")
}

func TestWrite_CollisionAfterExtensionCorrection(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "sunshine.png"), []byte("existing"), 0o644))

	res, err := Write(root, "sunshine.jpg", pngData, "image/png")
	require.NoError(t, err)
	assert.Equal(t, "sunshine-1.png", res.RelPath)
	assert.True(t, res.ExtensionCorrected)
}

func TestWrite_CollisionExhaustionReturnsErrNameExhausted(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "pic.png"), []byte("x"), 0o644))
	for n := 1; n < maxNameAttempts; n++ {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("pic-%d.png", n)), []byte("x"), 0o644))
	}

	_, err := Write(root, "pic.png", pngData, "image/png")
	require.ErrorIs(t, err, ErrNameExhausted)
}

func TestWrite_ConcurrentSameNameWriters(t *testing.T) {
	root := t.TempDir()
	const writers = 16

	results := make([]Result, writers)
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			results[i], errs[i] = Write(root, "pic.png", fmt.Appendf(nil, "content-%d", i), "image/png")
		})
	}
	wg.Wait()

	seen := make(map[string]bool, writers)
	for i := range writers {
		require.NoError(t, errs[i], "writer %d", i)
		assert.False(t, seen[results[i].RelPath], "writer %d got duplicate path %q", i, results[i].RelPath)
		seen[results[i].RelPath] = true
		assert.Equal(t, fmt.Appendf(nil, "content-%d", i), readWorkspaceFile(t, root, results[i].RelPath))
	}
	assert.True(t, seen["pic.png"])
	for i := 1; i < writers; i++ {
		assert.True(t, seen[fmt.Sprintf("pic-%d.png", i)])
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("stream torn") }

func TestWrite_FailedWriteRemovesTempAndKeepsExisting(t *testing.T) {
	root := t.TempDir()
	existing := []byte("keep-me")
	require.NoError(t, os.WriteFile(filepath.Join(root, "pic.png"), existing, 0o644))

	_, err := write(root, "pic.png", failingReader{}, "image/png")
	require.ErrorContains(t, err, "stream torn")

	// No final name exists until the complete temp is ready to publish.
	assert.Equal(t, existing, readWorkspaceFile(t, root, "pic.png"))
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "pic.png", entries[0].Name())

	emptyRoot := t.TempDir()
	_, err = write(emptyRoot, "pic.png", failingReader{}, "image/png")
	require.ErrorContains(t, err, "stream torn")
	assert.NoFileExists(t, filepath.Join(emptyRoot, "pic.png"))
	entries, err = os.ReadDir(emptyRoot)
	require.NoError(t, err)
	assert.Empty(t, entries, "failed writes must not leave a final or temp file")
}

func TestWrite_MissingWorkspaceRootFails(t *testing.T) {
	_, err := Write(filepath.Join(t.TempDir(), "gone"), "pic.png", pngData, "image/png")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrPathEscape)
}

func TestWrite_ParentDirIsFileFails(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "images"), []byte("file"), 0o644))

	_, err := Write(root, "images/pic.png", pngData, "image/png")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrPathEscape)
}
