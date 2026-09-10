package server

import (
	"errors"
	"net"

	winio "github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func isConnectionRefused(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED)
}

func listenNamedPipe(path string) (net.Listener, error) {
	return winio.ListenPipe(path, nil)
}
