//go:build !windows && !js

package desktop

import (
	"context"
	"net"
	"os"
	"strings"
)

func dialBackend(ctx context.Context) (net.Conn, error) {
	return dial(ctx, Paths().BackendSocket)
}

func dial(ctx context.Context, path string) (net.Conn, error) {
	dialer := net.Dialer{}

	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil && strings.Contains(err.Error(), "invalid argument") {
		// The socket path is too long: unix domain socket paths are limited to:
		// * 104 - 1 (NULL) characters on macOS
		// * 108 - 1 (NULL) characters on Linux.
		// Let's create a symlink to shorten the path.
		shortPath := "/tmp/docker_cagent.sock"
		_ = os.Remove(shortPath)
		if errLn := os.Symlink(path, shortPath); errLn != nil {
			return nil, err // return the original error
		}

		return dialer.DialContext(ctx, "unix", shortPath)
	}

	return conn, err
}
