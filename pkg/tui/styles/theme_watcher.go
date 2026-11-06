package styles

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ThemeWatcher reports theme file changes without applying them. It holds only
// configuration; each Run owns its watcher and can run independently.
type ThemeWatcher struct {
	onThemeChanged func(context.Context, string)

	// themesDir overrides ThemesDir in tests.
	themesDir string
}

// NewThemeWatcher creates a watcher whose callback should notify the UI goroutine.
func NewThemeWatcher(onThemeChanged func(context.Context, string)) *ThemeWatcher {
	return &ThemeWatcher{onThemeChanged: onThemeChanged}
}

// Run watches a user theme until ctx is canceled. Built-in themes without a user
// override and missing files are no-ops. Cancellation returns nil.
// Ready is called after attachment, before any notifications; no-op runs skip it.
// The callbacks run synchronously and must return when the context is canceled.
func (tw *ThemeWatcher) Run(ctx context.Context, themeRef string, ready func()) error {
	if ctx.Err() != nil {
		return nil
	}
	if themeRef == DefaultThemeRef {
		return nil
	}

	themePath, err := tw.findThemePath(themeRef)
	if err != nil {
		slog.DebugContext(ctx, "No user theme file found, not watching", "theme", themeRef)
		return nil
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	// Watch the directory so atomic saves (write then rename) are detected too.
	if err := watcher.Add(filepath.Dir(themePath)); err != nil {
		return err
	}

	if ready != nil {
		ready()
	}
	slog.DebugContext(ctx, "Started watching theme file", "theme", themeRef, "path", themePath)
	tw.watchLoop(ctx, themeRef, themePath, watcher)
	return nil
}

func (tw *ThemeWatcher) getThemesDir() string {
	if tw.themesDir != "" {
		return tw.themesDir
	}
	return ThemesDir()
}

func (tw *ThemeWatcher) findThemePath(themeRef string) (string, error) {
	dir := tw.getThemesDir()
	baseRef := strings.TrimPrefix(themeRef, UserThemePrefix)

	yamlPath := filepath.Join(dir, baseRef+".yaml")
	if _, err := os.Stat(yamlPath); err == nil {
		return yamlPath, nil
	}

	ymlPath := filepath.Join(dir, baseRef+".yml")
	if _, err := os.Stat(ymlPath); err == nil {
		return ymlPath, nil
	}

	return "", os.ErrNotExist
}

func (tw *ThemeWatcher) watchLoop(ctx context.Context, themeRef, themePath string, watcher *fsnotify.Watcher) {
	var timer *time.Timer
	var debounce <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return

		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			eventPath := filepath.Clean(event.Name)
			isExactMatch := eventPath == themePath
			isBasenameMatch := filepath.Base(eventPath) == filepath.Base(themePath)
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			if !isExactMatch && (!isBasenameMatch || event.Op&(fsnotify.Rename|fsnotify.Create) == 0) {
				continue
			}

			if timer == nil {
				timer = time.NewTimer(500 * time.Millisecond)
			} else {
				timer.Reset(500 * time.Millisecond)
			}
			debounce = timer.C

		case <-debounce:
			debounce = nil
			if ctx.Err() != nil {
				return
			}
			if _, err := os.Stat(themePath); err == nil && tw.onThemeChanged != nil {
				slog.DebugContext(ctx, "Theme file changed, signaling", "theme", themeRef)
				tw.onThemeChanged(ctx, themeRef)
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			slog.WarnContext(ctx, "Theme file watcher error", "error", err)
		}
	}
}
