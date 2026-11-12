// Package editorname resolves the user's configured external editor (VISUAL
// or EDITOR): a friendly display name used in TUI key-binding hints, and the
// command that launches the editor over a file.
//
// The environment-reading entry points are thin wrappers over pure functions
// (FromEnv, CommandFromEnv) that take the raw variable values as parameters,
// so every code path is testable across platforms and editor configurations
// without touching the actual process environment.
package editorname

import (
	"cmp"
	"os"
	"os/exec"
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

	var executable string
	for part := range strings.FieldsSeq(editorCmd) {
		executable = part
		break
	}
	if executable == "" {
		return "$EDITOR"
	}

	baseName := filepath.Base(executable)

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

// Command builds the *exec.Cmd that opens path in the configured external
// editor, reading VISUAL and EDITOR from the process environment.
func Command(path string) *exec.Cmd {
	return CommandFromEnv(os.Getenv("VISUAL"), os.Getenv("EDITOR"), path)
}

// CommandFromEnv builds the editor command from raw environment values so
// tests can exercise every code path without mutating os.Environ. VISUAL
// wins over EDITOR; the chosen value is split on whitespace (strings.Fields,
// deliberately no shell evaluation), extra tokens become leading arguments,
// and path is appended last. When neither variable yields a command, the
// platform default is launched ("notepad" on Windows, "vi" elsewhere).
//
// Stdin/Stdout/Stderr are bound to the real terminal files: left nil,
// tea.ExecProcess fills them from the Program, whose output is the
// non-*os.File image-writer wrapper — the editor would then see a pipe
// instead of a TTY ("Vim: Warning: Output is not to a terminal").
func CommandFromEnv(visual, editorEnv, path string) *exec.Cmd {
	parts := strings.Fields(cmp.Or(visual, editorEnv))
	if len(parts) == 0 {
		if goruntime.GOOS == "windows" {
			parts = []string{"notepad"}
		} else {
			parts = []string{"vi"}
		}
	}
	args := append(parts[1:], path)
	// The editor process is owned by tea.ExecProcess, so exec.Command is intentional.
	cmd := exec.Command(parts[0], args...) //nolint:noctx // owned by tea.ExecProcess
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}
