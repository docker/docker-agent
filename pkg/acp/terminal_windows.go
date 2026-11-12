package acp

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

func terminalInterpreter() (string, []string) {
	// Use the trusted system directory, not cwd/PATH/ComSpec. Client paths must match.
	directory, err := windows.GetSystemDirectory()
	if err != nil || !filepath.IsAbs(directory) {
		return "", nil
	}
	return filepath.Join(directory, "cmd.exe"), []string{"/D", "/C"}
}
