// Package testutil holds helpers shared by tests.
package testutil

import (
	"os"
	"path/filepath"
	"runtime"
)

// tempHomeEnv marks a process tree that already runs with a temporary home.
const tempHomeEnv = "TESTUTIL_TEMP_HOME"

// xdgVariables redirect state away from HOME when set by the host.
var xdgVariables = []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"}

// ClearXDG unsets the XDG base-directory variables so tests that isolate state
// via HOME keep resolving under it. Call it from TestMain in packages with a
// custom TestMain; RunWithTempHome already does this.
func ClearXDG() {
	for _, name := range xdgVariables {
		_ = os.Unsetenv(name)
	}
}

// EnvSetter is implemented by *testing.T and *testing.B.
type EnvSetter interface{ Setenv(key, value string) }

// DefaultDirs returns the config, data and cache directories of a fresh
// install on goos. Spelled out rather than derived from pkg/paths, so a
// regression in any platform branch is caught.
func DefaultDirs(goos, home string) (config, data, cache string) {
	switch goos {
	case "darwin":
		support := filepath.Join(home, "Library", "Application Support", "cagent")
		return support, support, filepath.Join(home, "Library", "Caches", "cagent")
	case "windows":
		local := filepath.Join(home, "AppData", "Local", "cagent")
		return filepath.Join(home, "AppData", "Roaming", "cagent"), local, local
	default:
		return filepath.Join(home, ".config", "cagent"),
			filepath.Join(home, ".local", "share", "cagent"),
			filepath.Join(home, ".cache", "cagent")
	}
}

// SetHome isolates state under home for a single test. HOME and USERPROFILE
// are not enough on Windows, where config and data resolve from the
// %APPDATA% and %LOCALAPPDATA% known folders, and XDG variables take
// precedence everywhere.
func SetHome(t EnvSetter, home string) {
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if runtime.GOOS == "windows" {
		t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
		t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	}
	for _, name := range xdgVariables {
		t.Setenv(name, "")
	}
}

// RunWithTempHome runs a test suite with the home directory pointing at an
// empty temporary directory, so tests neither read nor write the developer's
// configuration and data. Call it from TestMain.
func RunWithTempHome(run func() int) int {
	// A test binary re-executed as a helper keeps the home its parent gave it:
	// helpers often exit without returning, which would leak their own.
	if os.Getenv(tempHomeEnv) != "" {
		return run()
	}

	home, err := os.MkdirTemp("", "docker-agent-test-home-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(home)

	restores := []func(){setenv(tempHomeEnv, home)}
	for _, name := range homeVariables(runtime.GOOS, os.Getenv("HOME")) {
		restores = append(restores, setenv(name, home))
	}
	if runtime.GOOS == "windows" {
		restores = append(restores,
			setenv("APPDATA", filepath.Join(home, "AppData", "Roaming")),
			setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local")),
		)
	}
	// XDG variables would otherwise point state at the developer's directories
	// instead of the temporary home with which these tests are isolated.
	for _, name := range xdgVariables {
		restores = append(restores, unsetenv(name))
	}
	defer func() {
		for _, restore := range restores {
			restore()
		}
	}()

	return run()
}

// homeVariables lists the environment variables that locate the home
// directory: the one os.UserHomeDir reads, plus HOME when it is set, because
// pkg/path prefers it on every platform.
func homeVariables(goos, home string) []string {
	switch {
	case goos != "windows":
		return []string{"HOME"}
	case home == "":
		// Introducing HOME would shadow the USERPROFILE that tests set on Windows.
		return []string{"USERPROFILE"}
	default:
		return []string{"USERPROFILE", "HOME"}
	}
}

// setenv sets an environment variable and returns a function that restores it.
func setenv(name, value string) (restore func()) {
	previous, wasSet := os.LookupEnv(name)
	if err := os.Setenv(name, value); err != nil {
		panic(err)
	}
	return func() {
		if wasSet {
			_ = os.Setenv(name, previous)
		} else {
			_ = os.Unsetenv(name)
		}
	}
}

// unsetenv removes an environment variable and returns a function that
// restores its previous value, if any.
func unsetenv(name string) (restore func()) {
	previous, wasSet := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		panic(err)
	}
	return func() {
		if wasSet {
			_ = os.Setenv(name, previous)
		} else {
			_ = os.Unsetenv(name)
		}
	}
}
