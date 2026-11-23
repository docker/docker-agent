// Package testutil holds helpers shared by tests.
package testutil

import (
	"os"
	"runtime"
	"testing"
)

// tempHomeEnv marks a process tree that already runs with a temporary home.
const tempHomeEnv = "TESTUTIL_TEMP_HOME"

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
func RequireTempHome(tb testing.TB) {
	tb.Helper()
	if os.Getenv(tempHomeEnv) == "" {
		tb.Fatal("tests must run with testutil.RunWithTempHome from TestMain")
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
