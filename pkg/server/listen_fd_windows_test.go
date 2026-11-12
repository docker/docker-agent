package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestCloseInheritedFile_NonSocket(t *testing.T) {
	t.Parallel()

	file, err := os.Create(filepath.Join(t.TempDir(), "not-a-socket"))
	require.NoError(t, err)
	defer file.Close()
	require.NoError(t, closeInheritedFile(file))
	require.ErrorIs(t, file.Close(), os.ErrClosed)
}

func TestListen_FD_ReleasesWinsockState(t *testing.T) {
	t.Parallel()

	for _, network := range []string{"tcp", "udp"} {
		for _, addr := range []string{"127.0.0.1:0", "0.0.0.0:0"} {
			t.Run(network+"/"+addr, func(t *testing.T) {
				t.Parallel()

				const childEnv = "DOCKER_AGENT_TEST_FD_CHILD"
				if os.Getenv(childEnv) != t.Name() {
					// A bad socket close must not taint other tests' Winsock state.
					executable, err := os.Executable()
					require.NoError(t, err)
					ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, executable, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.timeout=20s", "-test.v")
					cmd.Env = append(os.Environ(), childEnv+"="+t.Name())
					cmd.WaitDelay = time.Second
					output, err := cmd.CombinedOutput()
					require.NoError(t, err, "socket lifecycle subprocess:\n%s", output)
					return
				}

				var file *os.File
				if network == "tcp" {
					original, err := Listen(t.Context(), addr)
					require.NoError(t, err)
					defer original.Close()
					file, err = original.(*net.TCPListener).File()
					require.NoError(t, err)
				} else {
					var lc net.ListenConfig
					original, err := lc.ListenPacket(t.Context(), network, addr)
					require.NoError(t, err)
					defer original.Close()
					file, err = original.(*net.UDPConn).File()
					require.NoError(t, err)
				}
				pinFDOwnership(file)
				handle := windows.Handle(file.Fd())
				ln, err := Listen(t.Context(), fmt.Sprintf("fd://%d", handle))
				if network == "tcp" {
					require.NoError(t, err)
					defer ln.Close()
				} else {
					require.ErrorContains(t, err, "is not a listener")
				}

				for range 4096 {
					// Retain events to encourage reuse of the socket's slot.
					event, err := windows.CreateEvent(nil, 0, 0, nil)
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, windows.CloseHandle(event)) })
					if event != handle {
						continue
					}
					const soType = 0x1008 // Winsock SO_TYPE is not exported by syscall.
					_, err = syscall.GetsockoptInt(syscall.Handle(event), syscall.SOL_SOCKET, soType)
					require.ErrorIs(t, err, windows.WSAENOTSOCK, "closed socket must not remain registered in Winsock")
					runtime.GC()
					require.NoError(t, windows.SetEvent(event), "the file finalizer must not close a recycled handle")
					return
				}
				t.Skip("Windows did not reuse the socket handle after 4096 event allocations")
			})
		}
	}
}
