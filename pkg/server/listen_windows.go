package server

import (
	"errors"
	"net"
	"os"

	winio "github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func closeInheritedFile(file *os.File) error {
	// os.NewFile closes with CloseHandle, which leaves stale Winsock state.
	// Pin the wrapper so its finalizer cannot close a reused handle later.
	err := windows.Closesocket(windows.Handle(file.Fd()))
	if errors.Is(err, windows.WSAENOTSOCK) {
		return file.Close()
	}
	pinFDOwnership(file)
	return err
}

func isConnectionRefused(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED)
}

func listenNamedPipe(path string) (net.Listener, error) {
	return winio.ListenPipe(path, nil)
}
