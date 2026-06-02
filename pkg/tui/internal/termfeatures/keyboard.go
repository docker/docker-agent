package termfeatures

import (
	"runtime"
	"strings"
)

// SupportsModifiedEnter returns true for terminals that can distinguish
// Shift+Enter from Enter even when they do not report Kitty keyboard flags.
// On macOS, we use the CoreGraphics API to detect modifier key state at the
// system level, so all terminals effectively support this.
func SupportsModifiedEnter(getenv func(string) string) bool {
	if runtime.GOOS == "darwin" {
		return true
	}

	if getenv == nil {
		return false
	}

	termProgram := strings.ToLower(getenv("TERM_PROGRAM"))
	term := strings.ToLower(getenv("TERM"))

	return termProgram == "wezterm" ||
		getenv("WEZTERM_PANE") != "" ||
		getenv("WEZTERM_UNIX_SOCKET") != "" ||
		strings.Contains(term, "wezterm")
}
