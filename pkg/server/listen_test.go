package server

import (
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListen_FD(t *testing.T) {
	t.Parallel()

	var lc net.ListenConfig
	orig, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer orig.Close()

	file, err := orig.(*net.TCPListener).File()
	require.NoError(t, err)
	defer file.Close()

	ln, err := Listen(t.Context(), fmt.Sprintf("fd://%d", file.Fd()))
	require.NoError(t, err)
	defer ln.Close()

	assert.NotNil(t, ln.Addr())
}

func TestListen_FD_InvalidNumber(t *testing.T) {
	t.Parallel()

	_, err := Listen(t.Context(), "fd://notanumber")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid file descriptor")
}

func TestListen_FD_NegativeNumber(t *testing.T) {
	t.Parallel()

	_, err := Listen(t.Context(), "fd://-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be > 2")
}

func TestListen_FD_ReservedDescriptors(t *testing.T) {
	t.Parallel()

	for _, fd := range []string{"0", "1", "2"} {
		_, err := Listen(t.Context(), "fd://"+fd)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be > 2")
	}
}

func TestListen_FD_InvalidDescriptor(t *testing.T) {
	t.Parallel()

	// Use a very high fd number that's unlikely to exist
	_, err := Listen(t.Context(), "fd://999999")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "file descriptor 999999")
}

// TestListen_TCP_IPv4 verifies that the default TCP listener binds to an
// IPv4 loopback address. Regression test for the listener being hard-coded
// to "tcp4" without that being the documented intent.
func TestListen_TCP_IPv4(t *testing.T) {
	t.Parallel()

	ln, err := Listen(t.Context(), "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok)
	assert.NotNil(t, tcpAddr.IP.To4())
}

// TestListen_TCP_IPv6 verifies that an IPv6 loopback bind succeeds. The
// listener used to force "tcp4" which made this fail with
// "address ::1: non-IPv4 address" on dual-stack hosts.
func TestListen_TCP_IPv6(t *testing.T) {
	t.Parallel()

	// Probe whether the host actually has IPv6 before asserting.
	var probeLC net.ListenConfig
	probe, probeErr := probeLC.Listen(t.Context(), "tcp6", "[::1]:0")
	if probeErr != nil {
		t.Skipf("host does not support IPv6 loopback: %v", probeErr)
	}
	_ = probe.Close()

	ln, err := Listen(t.Context(), "[::1]:0")
	require.NoError(t, err)
	defer ln.Close()

	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok)
	assert.True(t, tcpAddr.IP.IsLoopback())
	assert.Nil(t, tcpAddr.IP.To4(), "expected an IPv6-only address")
}
