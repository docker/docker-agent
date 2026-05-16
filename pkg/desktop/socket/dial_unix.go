//go:build !windows

package socket

import (
	"context"
	"net"
)

// DialUnix is a simple wrapper for `net.Dial("unix")`.
func DialUnix(ctx context.Context, path string) (net.Conn, error) {
	dialer := &net.Dialer{}
	return dialer.DialContext(ctx, "unix", stripUnixScheme(path))
}
