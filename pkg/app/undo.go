package app

import (
	"context"
	"errors"
	"fmt"
	"os"
)

var ErrNothingToUndo = errors.New("nothing to undo")

type UndoSnapshotResult struct {
	RestoredFiles int
}

// SnapshotsEnabled reports whether automatic shadow-git snapshots are
// active. The answer is a controller-level capability check and does
// not depend on having an active session attached.
func (a *App) SnapshotsEnabled() bool {
	return a.snapshotController != nil && a.snapshotController.Enabled()
}

// UndoLastSnapshot restores the files captured in the most recent
// snapshot checkpoint for the current session.
func (a *App) UndoLastSnapshot(ctx context.Context) (UndoSnapshotResult, error) {
	if a.snapshotController == nil || a.session == nil {
		return UndoSnapshotResult{}, ErrNothingToUndo
	}
	return snapshotResult(a.snapshotController.UndoLast(ctx, a.session.ID, a.snapshotCwd()))
}

// ListSnapshots returns the file count of every snapshot captured during
// the current session, oldest first. Returns nil when no snapshots exist
// or when no controller is configured.
func (a *App) ListSnapshots() []int {
	if a.snapshotController == nil || a.session == nil {
		return nil
	}
	infos := a.snapshotController.List(a.session.ID)
	counts := make([]int, len(infos))
	for i, info := range infos {
		counts[i] = info.Files
	}
	return counts
}

// ResetSnapshot reverts every checkpoint past index keep so the workspace
// returns to the state captured at that snapshot. keep == 0 resets to
// the original pre-agent state.
func (a *App) ResetSnapshot(ctx context.Context, keep int) (UndoSnapshotResult, error) {
	if a.snapshotController == nil || a.session == nil {
		return UndoSnapshotResult{}, ErrNothingToUndo
	}
	return snapshotResult(a.snapshotController.Reset(ctx, a.session.ID, a.snapshotCwd(), keep))
}

// snapshotCwd resolves the working directory the snapshot operations
// should run against. Sessions carry their own WorkingDir (set by the
// embedder when the session is constructed); if it's empty we fall
// back to os.Getwd so snapshot commands keep working in setups that
// don't propagate a working dir on the session.
func (a *App) snapshotCwd() string {
	if a.session != nil && a.session.WorkingDir != "" {
		return a.session.WorkingDir
	}
	cwd, _ := os.Getwd()
	return cwd
}

// snapshotResult adapts the (files, ok, err) tuple returned by snapshot
// operations into the UndoSnapshotResult / ErrNothingToUndo shape callers
// expect.
func snapshotResult(files int, ok bool, err error) (UndoSnapshotResult, error) {
	if err != nil {
		return UndoSnapshotResult{}, fmt.Errorf("restoring snapshot: %w", err)
	}
	if !ok {
		return UndoSnapshotResult{}, ErrNothingToUndo
	}
	return UndoSnapshotResult{RestoredFiles: files}, nil
}
