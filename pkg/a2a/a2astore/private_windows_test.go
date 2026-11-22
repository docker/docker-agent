package a2astore

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// TestStoreFilesAreOwnerOnly checks the directory drops inherited ACEs and
// that every SQLite file, checked while the store is open, grants only the
// current user.
func TestStoreFilesAreOwnerOnly(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "a2a")

	s, err := New(ctx, dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	_, err = s.Save(ctx, "task-1", &a2a.PushConfig{URL: "https://hook.example/cb", Token: "sekret"})
	require.NoError(t, err)

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(t, err)

	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	control, _, err := sd.Control()
	require.NoError(t, err)
	assert.NotZero(t, control&windows.SE_DACL_PROTECTED, "directory must not inherit the parent's ACEs")
	assertOnlyUser(t, dir, user.User.Sid)

	seen := 0
	for _, p := range sqliteFiles(filepath.Join(dir, dbFile)) {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			continue
		}
		seen++
		assertOnlyUser(t, p, user.User.Sid)
	}
	assert.Equal(t, 3, seen, "db, -wal and -shm must exist while the store is open")
}

func assertOnlyUser(t *testing.T, path string, sid *windows.SID) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	dacl, _, err := sd.DACL()
	require.NoError(t, err)
	require.NotNil(t, dacl, "a nil DACL grants everyone access")
	require.EqualValuesf(t, 1, dacl.AceCount, "%s must have a single ACE", path)

	var ace *windows.ACCESS_ALLOWED_ACE
	require.NoError(t, windows.GetAce(dacl, 0, &ace))
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	assert.Truef(t, aceSID.Equals(sid), "%s ACE must be for the current user", path)
}
