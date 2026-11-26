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

// fileAllAccess is FILE_ALL_ACCESS, which x/sys/windows does not export.
const fileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff

var procGetEffectiveRightsFromACL = windows.NewLazySystemDLL("advapi32.dll").NewProc("GetEffectiveRightsFromAclW")

// TestStoreFilesAreOwnerOnly checks the directory and every SQLite file,
// while the store is open, for a fresh store and for one reopened over
// permissive ACLs left by an earlier run.
func TestStoreFilesAreOwnerOnly(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(t, err)
	sid := user.User.Sid

	t.Run("fresh", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "a2a")
		openAndCheck(t, dir, sid)
	})

	t.Run("reopened with permissive ACLs", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "a2a")
		s, err := New(t.Context(), dir)
		require.NoError(t, err)
		require.NoError(t, s.Close())

		db := filepath.Join(dir, dbFile)
		require.NoError(t, os.WriteFile(db+"-wal", nil, 0o600))
		for _, p := range []string{dir, db, db + "-wal"} {
			grantEveryone(t, p, sid)
			require.NotEmpty(t, foreignRights(t, p), "%s must start out permissive", p)
		}
		openAndCheck(t, dir, sid)
	})
}

func openAndCheck(t *testing.T, dir string, sid *windows.SID) {
	t.Helper()
	ctx := t.Context()
	s, err := New(ctx, dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	_, err = s.Save(ctx, "task-1", &a2a.PushConfig{URL: "https://hook.example/cb", Token: "sekret"})
	require.NoError(t, err)

	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	control, _, err := sd.Control()
	require.NoError(t, err)
	assert.NotZero(t, control&windows.SE_DACL_PROTECTED, "directory must not inherit the parent's ACEs")
	assertOwnerOnly(t, dir, sid, true)

	seen := 0
	for _, p := range sqliteFiles(filepath.Join(dir, dbFile)) {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			continue
		}
		seen++
		assertOwnerOnly(t, p, sid, false)
	}
	assert.Equal(t, 3, seen, "db, -wal and -shm must exist while the store is open")
}

// assertOwnerOnly checks every ACE, not their count: SetEntriesInAcl splits
// an inheritable generic grant into an effective ACE and an inherit-only one.
func assertOwnerOnly(t *testing.T, path string, sid *windows.SID, isDir bool) {
	t.Helper()
	dacl := daclOf(t, path)

	var effective, inheritable bool
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		require.NoError(t, windows.GetAce(dacl, i, &ace))
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		assert.EqualValuesf(t, windows.ACCESS_ALLOWED_ACE_TYPE, ace.Header.AceType, "%s ACE %d type", path, i)
		assert.Truef(t, aceSID.Equals(sid), "%s ACE %d grants %s, not the current user", path, i, aceSID)

		flags := ace.Header.AceFlags
		full := ace.Mask&windows.GENERIC_ALL != 0 || ace.Mask&fileAllAccess == fileAllAccess
		if flags&windows.INHERIT_ONLY_ACE == 0 && full {
			effective = true
		}
		if flags&windows.OBJECT_INHERIT_ACE != 0 && flags&windows.CONTAINER_INHERIT_ACE != 0 {
			inheritable = true
		}
	}
	assert.Truef(t, effective, "%s must grant the current user full access", path)
	if isDir {
		assert.True(t, inheritable, "directory ACE must be inherited by new files")
	}
	assert.Emptyf(t, foreignRights(t, path), "%s grants access to other principals", path)
}

func daclOf(t *testing.T, path string) *windows.ACL {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	dacl, _, err := sd.DACL()
	require.NoError(t, err)
	require.NotNil(t, dacl, "a nil DACL grants everyone access")
	return dacl
}

// foreignRights returns the effective rights broad groups hold on path.
func foreignRights(t *testing.T, path string) map[string]uint32 {
	t.Helper()
	dacl := daclOf(t, path)
	rights := map[string]uint32{}
	for _, kind := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinWorldSid, windows.WinAuthenticatedUserSid,
		windows.WinBuiltinUsersSid, windows.WinBuiltinAdministratorsSid,
	} {
		group, err := windows.CreateWellKnownSid(kind)
		require.NoError(t, err)
		trustee := windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(group),
		}
		var mask uint32
		r, _, _ := procGetEffectiveRightsFromACL.Call(
			uintptr(unsafe.Pointer(dacl)), uintptr(unsafe.Pointer(&trustee)), uintptr(unsafe.Pointer(&mask)))
		require.Zero(t, r, "GetEffectiveRightsFromAcl")
		if mask != 0 {
			rights[group.String()] = mask
		}
	}
	return rights
}

// grantEveryone replaces path's DACL with an unprotected one that also lets
// Everyone in, as a permissive earlier setup would.
func grantEveryone(t *testing.T, path string, owner *windows.SID) {
	t.Helper()
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(t, err)
	entry := func(sid *windows.SID, kind windows.TRUSTEE_TYPE) windows.EXPLICIT_ACCESS {
		return windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  kind,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		}
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		entry(owner, windows.TRUSTEE_IS_USER), entry(everyone, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
	}, nil)
	require.NoError(t, err)
	require.NoError(t, windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil))
}
