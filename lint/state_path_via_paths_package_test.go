package main

import (
	"testing"

	"github.com/dgageot/rubocop-go/coptest"
	"github.com/stretchr/testify/assert"
)

func TestStatePathViaPathsPackageFlagsHardcodedCagent(t *testing.T) {
	t.Parallel()
	src := `package mypackage
import "path/filepath"
func stateDir() string {
	return filepath.Join(homeDir, ".cagent", "history")
}
`
	offenses := coptest.RunNamed(t, StatePathViaPathsPackage, "pkg/history/history.go", src)
	assert.Len(t, offenses, 1)
	assert.Equal(t, "Lint/StatePathViaPathsPackage", offenses[0].CopName)
}

func TestStatePathViaPathsPackageFlagsDirectDotCagent(t *testing.T) {
	t.Parallel()
	src := `package mypackage
const dir = ".cagent"
`
	offenses := coptest.RunNamed(t, StatePathViaPathsPackage, "pkg/content/store.go", src)
	assert.Len(t, offenses, 1)
}

func TestStatePathViaPathsPackageAllowsPathsPackage(t *testing.T) {
	t.Parallel()
	src := `package paths
const dotCagent = ".cagent"
`
	// Scoped away from pkg/paths
	assert.Empty(t, coptest.RunNamed(t, StatePathViaPathsPackage, "pkg/paths/paths.go", src))
}

func TestStatePathViaPathsPackageAllowsTests(t *testing.T) {
	t.Parallel()
	src := `package mypackage
const expected = ".cagent"
`
	assert.Empty(t, coptest.RunNamed(t, StatePathViaPathsPackage, "pkg/history/history_test.go", src))
}

func TestStatePathViaPathsPackageIgnoresUnrelatedStrings(t *testing.T) {
	t.Parallel()
	src := `package mypackage
const dir = "something-else"
`
	assert.Empty(t, coptest.RunNamed(t, StatePathViaPathsPackage, "pkg/foo/foo.go", src))
}
