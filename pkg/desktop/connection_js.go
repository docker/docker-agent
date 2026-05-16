//go:build js && wasm

package desktop

import (
	"context"
	"errors"
	"net"
)

// errNoDesktopUnderWasm is returned by every desktop-socket dial under
// js/wasm. The browser cannot dial Unix sockets or Windows named pipes, so
// any code path that tries to talk to Docker Desktop simply returns this
// error and lets the caller skip the Desktop-integration path.
var errNoDesktopUnderWasm = errors.New("docker desktop integration is not available under js/wasm")

func dialBackend(_ context.Context) (net.Conn, error) {
	return nil, errNoDesktopUnderWasm
}

func dial(_ context.Context, _ string) (net.Conn, error) {
	return nil, errNoDesktopUnderWasm
}
