package styles

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestThemeWatcher creates a theme watcher with a custom themes directory for testing.
func newTestThemeWatcher(themesDir string, onThemeChanged func(themeRef string)) *ThemeWatcher {
	tw := NewThemeWatcher(onThemeChanged)
	tw.themesDir = themesDir
	return tw
}

func TestThemeWatcher_WatchUserTheme(t *testing.T) {
	t.Parallel()

	// Create a temporary theme file
	tempDir := t.TempDir()
	themePath := filepath.Join(tempDir, "test-theme.yaml")
	initialContent := `version: 1
name: Test Theme
colors:
  accent_blue: "#FF0000"
`
	require.NoError(t, os.WriteFile(themePath, []byte(initialContent), 0o644))

	// Track callback invocations
	var callbackCount atomic.Int32
	var lastThemeRef atomic.Value
	callback := func(themeRef string) {
		callbackCount.Add(1)
		lastThemeRef.Store(themeRef)
	}

	// Create watcher with custom themes directory
	watcher := newTestThemeWatcher(tempDir, callback)
	defer watcher.Stop()

	// Start watching
	err := watcher.Watch("test-theme")
	require.NoError(t, err)

	// Modify the theme file
	updatedContent := `version: 1
name: Updated Theme
colors:
  accent_blue: "#00FF00"
`
	require.NoError(t, os.WriteFile(themePath, []byte(updatedContent), 0o644))

	// Wait for the watcher to detect the change and fire the callback
	require.Eventually(t, func() bool {
		return callbackCount.Load() >= 1
	}, 5*time.Second, 50*time.Millisecond, "callback should have been called at least once")
	ref, ok := lastThemeRef.Load().(string)
	assert.True(t, ok)
	assert.Equal(t, "test-theme", ref, "callback should receive the correct theme ref")
}

func TestThemeWatcher_DoesNotWatchDefaultTheme(t *testing.T) {
	t.Parallel()

	var callbackCount atomic.Int32
	callback := func(themeRef string) {
		callbackCount.Add(1)
	}

	watcher := NewThemeWatcher(callback)
	defer watcher.Stop()

	// Try to watch the default theme (built-in, no file to watch)
	err := watcher.Watch(DefaultThemeRef)
	require.NoError(t, err)

	// The watcher should not be active for the built-in default theme
	watcher.mu.Lock()
	isWatching := watcher.watcher != nil
	watcher.mu.Unlock()

	assert.False(t, isWatching, "should not watch built-in default theme")
}

func TestThemeWatcher_WatchesUserOverrideOfBuiltin(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()

	// Create a user theme file with a built-in name
	themePath := filepath.Join(tempDir, "tokyo-night.yaml")
	require.NoError(t, os.WriteFile(themePath, []byte("version: 1\nname: My Tokyo Night\n"), 0o644))

	var callbackCount atomic.Int32
	callback := func(themeRef string) {
		callbackCount.Add(1)
	}

	// Create watcher with custom themes directory
	watcher := newTestThemeWatcher(tempDir, callback)
	defer watcher.Stop()

	// Watch tokyo-night - since user file exists, it should be watched
	err := watcher.Watch("tokyo-night")
	require.NoError(t, err)

	watcher.mu.Lock()
	isWatching := watcher.watcher != nil
	currentPath := watcher.currentPath
	watcher.mu.Unlock()

	assert.True(t, isWatching, "should watch user override of built-in theme")
	assert.Equal(t, themePath, currentPath)
}

func TestThemeWatcher_StopCleansUp(t *testing.T) {
	t.Parallel()

	// Create a temporary theme
	tempDir := t.TempDir()
	themePath := filepath.Join(tempDir, "cleanup-test.yaml")
	require.NoError(t, os.WriteFile(themePath, []byte("version: 1\nname: Test\n"), 0o644))

	// Create watcher with custom themes directory
	watcher := newTestThemeWatcher(tempDir, nil)

	// Start watching
	err := watcher.Watch("cleanup-test")
	require.NoError(t, err)

	// Verify watcher is active
	watcher.mu.Lock()
	isActive := watcher.watcher != nil
	watcher.mu.Unlock()
	assert.True(t, isActive, "watcher should be active")

	// Stop and verify cleanup
	watcher.Stop()

	watcher.mu.Lock()
	isActiveAfterStop := watcher.watcher != nil
	watcher.mu.Unlock()
	assert.False(t, isActiveAfterStop, "watcher should be stopped")
}

func TestThemeWatcher_SwitchingThemesUpdatesWatcher(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()

	// Create two theme files
	theme1Path := filepath.Join(tempDir, "theme1.yaml")
	theme2Path := filepath.Join(tempDir, "theme2.yaml")
	require.NoError(t, os.WriteFile(theme1Path, []byte("version: 1\nname: Theme1\n"), 0o644))
	require.NoError(t, os.WriteFile(theme2Path, []byte("version: 1\nname: Theme2\n"), 0o644))

	// Create watcher with custom themes directory
	watcher := newTestThemeWatcher(tempDir, nil)
	defer watcher.Stop()

	// Watch first theme
	err := watcher.Watch("theme1")
	require.NoError(t, err)

	watcher.mu.Lock()
	path1 := watcher.currentPath
	watcher.mu.Unlock()
	assert.Equal(t, theme1Path, path1)

	// Switch to second theme
	err = watcher.Watch("theme2")
	require.NoError(t, err)

	watcher.mu.Lock()
	path2 := watcher.currentPath
	watcher.mu.Unlock()
	assert.Equal(t, theme2Path, path2)
}

func TestThemeWatcher_SignalsOnAnyFileChange(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	themePath := filepath.Join(tempDir, "signal-test.yaml")
	require.NoError(t, os.WriteFile(themePath, []byte("version: 1\nname: Valid\n"), 0o644))

	var callbackCount atomic.Int32
	callback := func(themeRef string) {
		callbackCount.Add(1)
	}

	// Create watcher with custom themes directory
	watcher := newTestThemeWatcher(tempDir, callback)
	defer watcher.Stop()

	err := watcher.Watch("signal-test")
	require.NoError(t, err)

	// Write any content (even invalid YAML) - watcher just signals, doesn't validate
	require.NoError(t, os.WriteFile(themePath, []byte("this is: [not: valid yaml"), 0o644))

	// Wait for the watcher to detect the change and fire the callback
	require.Eventually(t, func() bool {
		return callbackCount.Load() >= 1
	}, 5*time.Second, 50*time.Millisecond, "callback should be called for any file change")
}
