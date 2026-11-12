//go:build windows

package sandbox

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"github.com/docker/docker-agent/pkg/atomicfile"
)

// Atomic replacement opens the destination with DELETE access.
// A plain os.Open cannot read this file because it does not share deletion.
func TestHashKitFileSharesDeletionWithPublisher(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	pathp, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	held, err := windows.CreateFile(pathp, windows.GENERIC_READ|windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	defer windows.CloseHandle(held)

	plain, err := os.Open(path)
	if plain != nil {
		plain.Close()
	}
	require.ErrorIs(t, err, windows.ERROR_SHARING_VIOLATION, "prove the ordinary-open regression")

	var hash bytes.Buffer
	require.NoError(t, hashKitFile(&hash, path))
	assert.NotEmpty(t, hash.Bytes())
}

func TestHashKitFileAllowsAtomicReplacementWhileOpen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	var oldHash bytes.Buffer
	require.NoError(t, hashKitFile(&oldHash, path))
	writer := &replaceDuringHash{path: path}
	require.NoError(t, hashKitFile(writer, path))
	require.NoError(t, writer.err, "the hash reader must not block an atomic publisher")
	assert.Equal(t, oldHash.Bytes(), writer.Bytes())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(data))
}

type replaceDuringHash struct {
	bytes.Buffer
	path string
	err  error
}

func (w *replaceDuringHash) Write(data []byte) (int, error) {
	w.err = atomicfile.Write(w.path, bytes.NewBufferString("new"), 0o600)
	return w.Buffer.Write(data)
}
