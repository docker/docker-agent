// Package shellpath provides safe shell binary resolution to prevent
// PATH hijacking attacks (CWE-426).
package shellpath

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ShellBaseName returns the lowercase shell name without extension. Splits on
// both separators so results are stable when the path came from another host OS.
func ShellBaseName(shellPath string) string {
	base := shellPath
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	return strings.ToLower(strings.TrimSuffix(base, filepath.Ext(base)))
}

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
	if path, ok := lookPowerShell(); ok {
		return path, []string{"-NoProfile", "-NonInteractive", "-Command"}
	}
	return WindowsCmdExe(), []string{"/C"}
}

// lookPowerShell resolves the preferred PowerShell binary (pwsh.exe first,
// then powershell.exe) via LookPath, which returns an absolute path.
func lookPowerShell() (string, bool) {
	for _, ps := range []string{"pwsh.exe", "powershell.exe"} {
		if path, err := exec.LookPath(ps); err == nil {
			return path, true
		}
	}
	return "", false
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

// InteractiveShellCmd returns a command that launches the user's preferred
// interactive shell, printing exitMsg first. The message is passed through an
// environment variable — never interpolated into the command line — so it
// cannot be reparsed as shell syntax. The command is owned by the caller
// (typically tea.ExecProcess), not by a request-scoped context, so
// exec.Command is intentional.
func InteractiveShellCmd(exitMsg string) *exec.Cmd {
	const msgVar = "DOCKER_AGENT_SHELL_EXIT_MSG"
	cmd := interactiveShellCmd(msgVar)
	cmd.Env = append(os.Environ(), msgVar+"="+exitMsg)
	return cmd
}

// interactiveShellCmd builds the platform-specific command that prints the
// message held in the msgVar environment variable, then hands over to an
// interactive shell.
func interactiveShellCmd(msgVar string) *exec.Cmd {
	if runtime.GOOS != "windows" {
		// printf (not `echo -e`) so the message prints verbatim under dash,
		// bash, zsh and fish alike.
		shell := DetectUnixShell()
		return execCmd(shell, "-i", "-c", `printf '\n%s\n' "$`+msgVar+`"; exec `+shell)
	}
	if path, ok := lookPowerShell(); ok {
		return execCmd(path, "-NoLogo", "-NoExit", "-Command", `Write-Host ""; Write-Host $env:`+msgVar)
	}
	// Absolute path to cmd.exe prevents PATH hijacking (CWE-426); /V:ON
	// delayed expansion (!VAR!) keeps the message out of cmd's parser.
	return execCmd(WindowsCmdExe(), "/V:ON", "/K", "echo. & echo !"+msgVar+"!")
}

// execCmd is a thin wrapper around exec.Command used for interactive
// processes whose lifecycle is owned by the caller (not a context).
func execCmd(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...) //nolint:noctx // owned by the caller (tea.ExecProcess)
}
