//go:build !windows

package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"syscall"
)

func closeInheritedFile(file *os.File) error {
	return file.Close()
}

func isConnectionRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

func listenNamedPipe(string) (net.Listener, error) {
	return nil, fmt.Errorf("named pipes not supported on %s", runtime.GOOS)
}
