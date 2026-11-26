package a2astore

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

const protectedDACL = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION

// preparePrivate gives dir a protected DACL granting access to the current
// user only. os.Chmod only toggles the read-only attribute on Windows; files
// SQLite creates in dir inherit this DACL instead of the parent's.
func preparePrivate(dir, dbPath string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}

	acl, err := ownerOnlyACL()
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, protectedDACL, nil, nil, acl, nil); err != nil {
		return fmt.Errorf("set directory ACL: %w", err)
	}
	// Files from an earlier run may carry explicit ACEs that inheritance doesn't replace.
	for _, p := range sqliteFiles(dbPath) {
		err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, protectedDACL, nil, nil, acl, nil)
		if err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return fmt.Errorf("set file ACL: %w", err)
		}
	}
	return nil
}

func ownerOnlyACL() (*windows.ACL, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("resolve current user: %w", err)
	}
	return windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, nil)
}
