// Package shellpath provides safe shell binary resolution to prevent
// PATH hijacking attacks (CWE-426).
package shellpath

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// WindowsCmdExe returns the absolute path to cmd.exe on Windows using the
// SystemRoot environment variable (e.g. C:\Windows\System32\cmd.exe).
// This avoids resolving cmd.exe through PATH, which would be vulnerable
// to untrusted search path attacks (CWE-426).
//
// If the ComSpec environment variable is set, its value is returned as-is
// (ComSpec is typically set by Windows itself to the correct cmd.exe path).
//
// As a last resort, if neither ComSpec nor SystemRoot is set, it falls back
// to the bare "cmd.exe" name (should never happen on a normal Windows system).
func WindowsCmdExe() string {
	if comspec := os.Getenv("ComSpec"); comspec != "" {
		return comspec
	}
	if systemRoot := os.Getenv("SystemRoot"); systemRoot != "" {
		return filepath.Join(systemRoot, "System32", "cmd.exe")
	}
	return "cmd.exe"
}

// DetectShell returns the appropriate shell binary and its argument prefix
// for the current platform.
//
// On Windows, it prefers PowerShell (pwsh.exe or powershell.exe) resolved
// via exec.LookPath (which returns an absolute path on success), falling
// back to cmd.exe resolved through [WindowsCmdExe].
//
// On Unix, it uses the SHELL environment variable or /bin/sh.
func DetectShell() (shell string, argsPrefix []string) {
	if runtime.GOOS == "windows" {
		return DetectWindowsShell()
	}

	return defaultUnixShell(), []string{"-c"}
}

// DetectWindowsShell returns the shell binary and argument prefix for Windows.
// It prefers PowerShell (resolved via LookPath, which returns an absolute path),
// falling back to cmd.exe via [WindowsCmdExe].
func DetectWindowsShell() (shell string, argsPrefix []string) {
	powershellArgs := []string{"-NoProfile", "-NonInteractive", "-Command"}
	for _, ps := range []string{"pwsh.exe", "powershell.exe"} {
		if path, err := exec.LookPath(ps); err == nil {
			return path, powershellArgs
		}
	}
	return WindowsCmdExe(), []string{"/C"}
}

// DetectUnixShell returns the user's shell from the SHELL environment variable,
// falling back to /bin/sh.
func DetectUnixShell() string {
	return defaultUnixShell()
}

// defaultUnixShell returns the user's shell from SHELL or /bin/sh.
func defaultUnixShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/sh"
}
