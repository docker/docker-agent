package shellpath

import (
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestShellBaseName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path     string
		expected string
	}{
		{path: "/bin/zsh", expected: "zsh"},
		{path: "/usr/local/bin/fish", expected: "fish"},
		{path: "/bin/sh", expected: "sh"},
		{path: `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, expected: "powershell"},
		{path: `C:\Program Files\PowerShell\7\pwsh.exe`, expected: "pwsh"},
		{path: `C:\Windows\System32\cmd.exe`, expected: "cmd"},
		{path: "", expected: ""},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			if got := ShellBaseName(tt.path); got != tt.expected {
				t.Errorf("ShellBaseName(%q) = %q, want %q", tt.path, got, tt.expected)
			}
		})
	}
}

func TestWindowsCmdExe_ComSpec(t *testing.T) {
	t.Setenv("ComSpec", `C:\Custom\cmd.exe`)
	got := WindowsCmdExe()
	if got != `C:\Custom\cmd.exe` {
		t.Errorf("WindowsCmdExe() = %q, want %q", got, `C:\Custom\cmd.exe`)
	}
}

func TestWindowsCmdExe_SystemRoot(t *testing.T) {
	t.Setenv("ComSpec", "")
	t.Setenv("SystemRoot", `C:\Windows`)
	got := WindowsCmdExe()
	want := `C:\Windows` + string(filepath.Separator) + filepath.Join("System32", "cmd.exe")
	if got != want {
		t.Errorf("WindowsCmdExe() = %q, want %q", got, want)
	}
}

func TestWindowsCmdExe_Fallback(t *testing.T) {
	t.Setenv("ComSpec", "")
	t.Setenv("SystemRoot", "")
	got := WindowsCmdExe()
	if got != "cmd.exe" {
		t.Errorf("WindowsCmdExe() = %q, want %q", got, "cmd.exe")
	}
}

func TestDetectShell_Unix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-only test")
	}

	t.Setenv("SHELL", "/bin/zsh")
	shell, args := DetectShell()
	if shell != "/bin/zsh" {
		t.Errorf("DetectShell() shell = %q, want /bin/zsh", shell)
	}
	if len(args) != 1 || args[0] != "-c" {
		t.Errorf("DetectShell() args = %v, want [-c]", args)
	}
}

func TestDetectShell_Unix_Fallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-only test")
	}

	t.Setenv("SHELL", "")
	shell, _ := DetectShell()
	if shell != "/bin/sh" {
		t.Errorf("DetectShell() shell = %q, want /bin/sh", shell)
	}
}

func TestDefaultUnixShell(t *testing.T) {
	t.Setenv("SHELL", "/usr/local/bin/fish")
	got := defaultUnixShell()
	if got != "/usr/local/bin/fish" {
		t.Errorf("defaultUnixShell() = %q, want /usr/local/bin/fish", got)
	}

	t.Setenv("SHELL", "")
	got = defaultUnixShell()
	if got != "/bin/sh" {
		t.Errorf("defaultUnixShell() = %q, want /bin/sh", got)
	}
}

func TestWindowsCmdExe_PrefersComSpecOverSystemRoot(t *testing.T) {
	// When both are set, ComSpec should take priority
	t.Setenv("ComSpec", `D:\Tools\cmd.exe`)
	t.Setenv("SystemRoot", `C:\Windows`)
	got := WindowsCmdExe()
	if got != `D:\Tools\cmd.exe` {
		t.Errorf("WindowsCmdExe() = %q, want %q (ComSpec should take priority)", got, `D:\Tools\cmd.exe`)
	}
}

func TestDetectWindowsShell_FallbackUsesAbsolutePath(t *testing.T) {
	// Simulate an environment where no PowerShell is found:
	// set PATH to empty so LookPath won't find pwsh.exe or powershell.exe
	t.Setenv("PATH", "")

	t.Setenv("ComSpec", "")
	t.Setenv("SystemRoot", `C:\Windows`)

	shell, args := DetectWindowsShell()
	want := `C:\Windows` + string(filepath.Separator) + filepath.Join("System32", "cmd.exe")
	if shell != want {
		t.Errorf("DetectWindowsShell() shell = %q, want %q", shell, want)
	}
	if len(args) != 1 || args[0] != "/C" {
		t.Errorf("DetectWindowsShell() args = %v, want [/C]", args)
	}
}

func TestInteractiveShellCmd_Unix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-only test")
	}

	t.Setenv("SHELL", "/bin/zsh")
	// A hostile message must stay an env value, never argv or shell syntax.
	msg := `pwned"; rm -rf $(HOME) &`
	cmd := InteractiveShellCmd(msg)

	want := []string{"/bin/zsh", "-i", "-c", `printf '\n%s\n' "$DOCKER_AGENT_SHELL_EXIT_MSG"; exec /bin/zsh`}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("InteractiveShellCmd() args = %q, want %q", cmd.Args, want)
	}
	if !slices.Contains(cmd.Env, "DOCKER_AGENT_SHELL_EXIT_MSG="+msg) {
		t.Errorf("InteractiveShellCmd() env is missing DOCKER_AGENT_SHELL_EXIT_MSG=%q", msg)
	}
}
