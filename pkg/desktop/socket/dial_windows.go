package socket

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
)

// DialUnix is a simple wrapper for `winio.DialPipe(path, 10s)`.
// It provides API compatibility for named pipes with the Unix domain socket API.
func DialUnix(ctx context.Context, path string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if strings.HasPrefix(path, "unix://") {
		// windows supports AF_UNIX
		d := &net.Dialer{}
		return d.DialContext(ctx, "unix", stripUnixScheme(path))
	}
	return winio.DialPipeContext(ctx, path)
}
