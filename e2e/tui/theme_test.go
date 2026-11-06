package tui_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/tuitest"
)

// TestTheme_HotReload is the regression net for the theme watcher wiring
// (issue #3501): editing the active custom theme file while the TUI runs
// must hot-reload it without a restart. It drives the full loop — switch to
// a user theme, edit its file on disk, and wait for the watcher to emit
// ThemeFileChangedMsg, which surfaces as the "Theme hot-reloaded"
// notification. No LLM call is made, so the cassette is empty.
func TestTheme_HotReload(t *testing.T) {
	// The theme registry is process-global; restore the default so other
	// tests never see this test's theme. Registered before newTUI so that
	// (LIFO) it runs only after the program stopped rendering.
	t.Cleanup(func() { styles.ApplyThemeRef(styles.DefaultThemeRef) })
	d := newTUI(t, "testdata/basic.yaml", 120, 40)

	// isolateState (inside newTUI) redirected the data dir to a temp dir, so
	// this writes the user theme where the running TUI expects it.
	themesDir := styles.ThemesDir()
	require.NoError(t, os.MkdirAll(themesDir, 0o755))
	themePath := filepath.Join(themesDir, "hotreload.yaml")
	require.NoError(t, os.WriteFile(themePath, []byte("version: 1\nname: Hot Reload Before\n"), 0o644))

	// Switching themes re-targets the watcher onto the new theme's file.
	d.Send(messages.ChangeThemeMsg{ThemeRef: "user:hotreload"}).
		WaitFor(tuitest.Contains("Theme changed to Hot Reload Before"))

	// Process a retarget synchronously so a single immediate edit exercises readiness.
	d.Send(messages.ThemeChangedMsg{})
	for _, name := range []string{"Hot Reload After", "Hot Reload Again"} {
		require.NoError(t, os.WriteFile(themePath, []byte("version: 1\nname: "+name+"\n"), 0o644))
		require.Eventually(t, func() bool {
			return styles.CurrentTheme().Name == name
		}, 5*time.Second, 50*time.Millisecond, "a single edit should hot-reload the theme")
		d.WaitFor(tuitest.Contains("Theme hot-reloaded"))
	}
}
