//go:build !windows

package acp

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// makeUnreadable revokes read access to path for the rest of the test.
// POSIX modes express this directly; root is skipped because it bypasses
// file permissions.
func makeUnreadable(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	require.NoError(t, os.Chmod(path, 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
}
