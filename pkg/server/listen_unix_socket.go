package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
)

var unixSocketListenMu sync.Mutex

type unixSocketDialer func(context.Context, string) (net.Conn, error)

type unixSocketRenamer func(string, string) (os.FileInfo, error)

func listenUnixSocket(ctx context.Context, path string) (net.Listener, error) {
	// Keep cooperating starts from observing the path between reclamation and bind.
	unixSocketListenMu.Lock()
	defer unixSocketListenMu.Unlock()

	if err := prepareUnixSocket(ctx, path, dialUnixSocket, renameUnixSocket); err != nil {
		return nil, err
	}

	var config net.ListenConfig
	return config.Listen(ctx, "unix", path)
}

func dialUnixSocket(ctx context.Context, path string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", path)
}

func renameUnixSocket(oldPath, newPath string) (os.FileInfo, error) {
	if err := os.Rename(oldPath, newPath); err != nil {
		return nil, err
	}
	return os.Lstat(newPath)
}

func prepareUnixSocket(ctx context.Context, path string, dial unixSocketDialer, rename unixSocketRenamer) error {
	// Portable pathname operations cannot exclude a noncooperating process from
	// replacing this entry. On a detected race, restore the moved entry without
	// clobbering the path; the rename can still make the path briefly absent.
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspecting unix socket %q: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to replace non-socket path %q", path)
	}

	conn, err := dial(ctx, path)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("unix socket %q is already in use", path)
	}
	if !isConnectionRefused(err) {
		return fmt.Errorf("checking unix socket %q: %w", path, err)
	}

	quarantineDir, err := os.MkdirTemp(filepath.Dir(path), ".docker-agent-stale-socket-")
	if err != nil {
		return fmt.Errorf("creating unix socket quarantine: %w", err)
	}
	quarantined := filepath.Join(quarantineDir, "socket")
	current, err := rename(path, quarantined)
	if err != nil {
		cleanupErr := os.Remove(quarantineDir)
		return errors.Join(fmt.Errorf("quarantining stale unix socket %q: %w", path, err), cleanupErr)
	}

	if !os.SameFile(info, current) {
		// A noncooperating process can replace the path between inspection and
		// rename. Restore its entry without overwriting anything created since.
		if err := restoreQuarantinedSocket(path, quarantined); err != nil {
			return fmt.Errorf("unix socket %q changed while checking it: %w", path, err)
		}
		if err := os.Remove(quarantineDir); err != nil {
			return fmt.Errorf("unix socket %q changed while checking it; replacement restored but removing quarantine directory: %w", path, err)
		}
		return fmt.Errorf("unix socket %q changed while checking it; replacement restored", path)
	}

	if err := os.Remove(quarantined); err != nil {
		return fmt.Errorf("removing stale unix socket %q preserved at %q: %w", path, quarantined, err)
	}
	if err := os.Remove(quarantineDir); err != nil {
		return fmt.Errorf("removing unix socket quarantine %q: %w", quarantineDir, err)
	}
	return nil
}

func restoreQuarantinedSocket(path, quarantined string) error {
	if err := os.Link(quarantined, path); err != nil {
		return fmt.Errorf("replacement preserved at %q (public path left unchanged): %w", quarantined, err)
	}
	if err := os.Remove(quarantined); err != nil {
		return fmt.Errorf("replacement restored at %q but also preserved at %q: %w", path, quarantined, err)
	}
	return nil
}
