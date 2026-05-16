package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func Listen(ctx context.Context, addr string) (net.Listener, error) {
	if path, ok := strings.CutPrefix(addr, "unix://"); ok {
		return listenUnix(ctx, path)
	}

	if path, ok := strings.CutPrefix(addr, "npipe://"); ok {
		return listenNamedPipe(path)
	}

	if raw, ok := strings.CutPrefix(addr, "fd://"); ok {
		return listenFD(raw)
	}

	return listenTCP(ctx, addr)
}

func listenFD(raw string) (net.Listener, error) {
	fd, err := strconv.Atoi(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid file descriptor %q: %w", raw, err)
	}
	if fd < 3 {
		return nil, fmt.Errorf("invalid file descriptor %d: must be > 2 (0-2 are stdin/stdout/stderr)", fd)
	}

	f := os.NewFile(uintptr(fd), "fd://"+raw)
	defer f.Close() // net.FileListener duplicates the fd; close our copy

	ln, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("file descriptor %d is not a listener: %w", fd, err)
	}

	return ln, nil
}

func listenUnix(ctx context.Context, path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // socket access is gated by socket file permissions, not directory
		return nil, err
	}

	var lnConfig net.ListenConfig
	return lnConfig.Listen(ctx, "unix", path)
}

func listenTCP(ctx context.Context, addr string) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp", addr)
}
