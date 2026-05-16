// Package editorname maps the user's configured external editor (VISUAL or
// EDITOR) to a friendly display name used in TUI key-binding hints.
//
// The lookup is intentionally a pure function so that it is trivial to test
// across platforms and editor configurations without touching the actual
// process environment.
package editorname

import (
	"cmp"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

// editorPrefixes maps a binary-name prefix (e.g. "code") to a friendly display
// name (e.g. "VSCode"). Order matters: longer / more specific prefixes must
// appear before shorter ones (e.g. "vim" before "vi").
var editorPrefixes = []struct {
	prefix string
	name   string
}{
	{"code", "VSCode"},
	{"cursor", "Cursor"},
	{"nvim", "Neovim"},
	{"vim", "Vim"},
	{"vi", "Vi"},
	{"nano", "Nano"},
	{"emacs", "Emacs"},
	{"subl", "Sublime Text"},
	{"sublime", "Sublime Text"},
	{"atom", "Atom"},
	{"gedit", "gedit"},
	{"kate", "Kate"},
	{"notepad++", "Notepad++"},
	{"notepad", "Notepad"},
	{"textmate", "TextMate"},
	{"mate", "TextMate"},
	{"zed", "Zed"},
}

// FromEnv returns a friendly display name for the configured external editor.
// It reads VISUAL first, then falls back to EDITOR. When neither is set, it
// returns the platform-specific fallback ("Notepad" on Windows, "Vi" elsewhere)
// that matches the actual command that will be launched.
//
// FromEnv is pure: it takes the raw environment values as parameters so that
// tests can exercise every code path without mutating os.Environ.
func FromEnv(visual, editorEnv string) string {
	editorCmd := cmp.Or(visual, editorEnv)
	if editorCmd == "" {
		if goruntime.GOOS == "windows" {
			return "Notepad"
		}
		return "Vi"
	}

	parts := strings.Fields(editorCmd)
	if len(parts) == 0 {
		return "$EDITOR"
	}

	baseName := filepath.Base(parts[0])

	for _, e := range editorPrefixes {
		if strings.HasPrefix(baseName, e.prefix) {
			return e.name
		}
	}

	if baseName != "" {
		// Capitalize the first rune (not byte) so that names beginning with
		// multi-byte UTF-8 characters survive the round-trip.
		r, size := utf8.DecodeRuneInString(baseName)
		if r != utf8.RuneError {
			return string(unicode.ToUpper(r)) + baseName[size:]
		}
	}

	return "$EDITOR"
}
