package strategy

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/docker/docker-agent/pkg/fsx"
)

const fileWatchDebounce = 2 * time.Second

type watchBatch struct {
	files, created []string
	rescan         bool
	initial        bool
}

// Keep draining events while registration or indexing blocks. One worker owns
// batches. Drain during cancellation too: some backends require event delivery
// to finish Add, and cannot safely close while an Add is awaiting its reply.
func watchFileChanges(ctx context.Context, watcher *fsnotify.Watcher, name string, docPaths []string, ignore func(string) bool, addPath func(context.Context, string) error, process func(context.Context, []string), rescan func(context.Context) error, closeWatcher func() error) error {
	ctx, cancel := context.WithCancel(ctx)
	jobs := make(chan watchBatch, 1)
	completed := make(chan struct{}, 1)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case batch := <-jobs:
				for _, path := range batch.created {
					if ctx.Err() != nil {
						return
					}
					if err := addPath(ctx, path); err != nil && ctx.Err() == nil {
						slog.WarnContext(ctx, "Failed to watch path", "strategy", name, "path", path, "error", err)
					}
					if info, err := os.Stat(path); err == nil && info.IsDir() && !batch.initial {
						batch.rescan = true
					}
				}
				if ctx.Err() != nil {
					return
				}
				if batch.rescan {
					if err := rescan(ctx); err != nil && ctx.Err() == nil {
						slog.ErrorContext(ctx, "Failed to reconcile watched files", "strategy", name, "error", err)
					}
				} else if len(batch.files) > 0 {
					process(ctx, batch.files)
				}
				completed <- struct{}{}
			}
		}
	}()
	defer func() {
		cancel()
		events, errs := watcher.Events, watcher.Errors
		for {
			select {
			case <-workerDone:
				_ = closeWatcher()
				return
			case _, ok := <-events:
				if !ok {
					events = nil
				}
			case _, ok := <-errs:
				if !ok {
					errs = nil
				}
			}
		}
	}()

	jobs <- watchBatch{created: docPaths, initial: true}
	busy := true
	var timer *time.Timer
	var debounce <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	pending, created := make(map[string]bool), make(map[string]bool)
	needsRescan, due := false, false
	arm := func() {
		due = false
		if timer == nil {
			timer = time.NewTimer(fileWatchDebounce)
		} else {
			timer.Reset(fileWatchDebounce)
		}
		debounce = timer.C
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if due && !busy {
			jobs <- watchBatch{files: slices.Sorted(maps.Keys(pending)), created: slices.Sorted(maps.Keys(created)), rescan: needsRescan}
			clear(pending)
			clear(created)
			needsRescan, due, busy = false, false, true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-completed:
			busy = false
		case <-debounce:
			debounce, due = nil, true
		case event, ok := <-watcher.Events:
			if !ok {
				return errors.New("file watcher events closed unexpectedly")
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			if event.Op&fsnotify.Create != 0 {
				created[event.Name] = true
				if debounce == nil && !due {
					arm()
				}
			}
			matches, err := fsx.Matches(event.Name, docPaths)
			if err != nil || !matches || (ignore != nil && ignore(event.Name)) {
				continue
			}
			pending[event.Name] = true
			arm()
		case err, ok := <-watcher.Errors:
			if !ok {
				return errors.New("file watcher errors closed unexpectedly")
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				for _, path := range docPaths {
					created[path] = true
				}
				needsRescan = true
				arm()
				continue
			}
			slog.ErrorContext(ctx, "File watcher error", "strategy", name, "error", err)
		}
	}
}
