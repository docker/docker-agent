// Package testutil holds helpers shared by tests.
package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/docker/docker-agent/pkg/paths"
)

// tempHomeEnv marks a process tree that already runs with a temporary home.
const tempHomeEnv = "TESTUTIL_TEMP_HOME"

// envValue is one environment variable assignment RunWithTempHome makes while
// a suite runs.
type envValue struct {
	name  string
	value string
}

// RunWithTempHome runs a test suite with the home directory and the effective
// cache root pointing at an empty temporary directory, so tests neither read
// nor write the developer's configuration, data, or cache. Call it from
// TestMain.
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
	for _, variable := range cacheVariables(runtime.GOOS, home) {
		restores = append(restores, setenv(variable.name, variable.value))
	}
	defer func() {
		for _, restore := range restores {
			restore()
		}
	}()

	return run()
}

// RequireTempHome fails unless the test binary runs under
// [RunWithTempHome]. State-touching packages call it from a test so that
// dropping the isolated TestMain fails loudly instead of writing to the
// developer's real home directory again.
//
// It also fails when the effective cache directory escapes the temporary
// home. The cache root does not always follow the home directory: Windows
// resolves it from %LocalAppData% and Linux from an absolute
// $XDG_CACHE_HOME, so a suite can keep writing the developer's cache while
// its home directory already looks isolated.
func RequireTempHome(tb testing.TB) {
	tb.Helper()
	home := os.Getenv(tempHomeEnv)
	if home == "" {
		tb.Fatal("tests must run with testutil.RunWithTempHome from TestMain")
	}
	if cache := paths.GetCacheDir(); !withinDir(home, cache) {
		tb.Fatalf("cache directory %q is outside the temporary home %q: testutil.RunWithTempHome must isolate the effective cache root, not just HOME", cache, home)
	}
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

// cacheVariables lists the environment variables that locate the platform's
// cache root, with the values that keep it inside home.
//
// os.UserCacheDir — and therefore [paths.GetCacheDir] — does not always derive
// the cache root from the home directory: it reads %LocalAppData% on Windows
// and an absolute $XDG_CACHE_HOME when set on Linux/BSD. Changing HOME
// alone leaves those pointing at the developer's real cache. On macOS the
// cache lives under $HOME/Library/Caches, so changing HOME already covers it.
func cacheVariables(goos, home string) []envValue {
	switch goos {
	case "windows":
		return []envValue{{name: "LocalAppData", value: filepath.Join(home, "AppData", "Local")}}
	case "darwin", "ios":
		return nil
	default:
		// A relative XDG_CACHE_HOME makes os.UserCacheDir fail, which would
		// send pkg/paths.GetCacheDir to the shared system temporary directory.
		return []envValue{{name: "XDG_CACHE_HOME", value: filepath.Join(home, ".cache")}}
	}
}

// withinDir reports whether path is dir or one of its descendants.
func withinDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && filepath.IsLocal(rel)
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
