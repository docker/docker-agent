//go:build windows

package acp

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// makeUnreadable revokes read access to path for the rest of the test.
// POSIX modes cannot express that on Windows (0o000 merely sets the
// read-only attribute), so the helper holds an exclusive handle - share
// mode 0 - until the test ends: any subsequent open fails with
// ERROR_SHARING_VIOLATION, while os.Stat keeps succeeding because
// metadata access is exempt from sharing checks.
func makeUnreadable(t *testing.T, path string) {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = windows.CloseHandle(h) })
}
