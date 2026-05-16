package builtins_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
)

// TestAddDirectoryListingIncludesFilesAndDirs verifies the happy path:
// regular files appear by name, directories get a trailing "/", and
// dot-files are excluded. We deliberately don't pin the header line
// format — the test is robust to header tweaks.
func TestAddDirectoryListingIncludesFilesAndDirs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "README.md", "")
	writeFile(t, dir, "main.go", "")
	writeFile(t, dir, ".env", "secret") // hidden — must be skipped
	require.NoError(t, os.Mkdir(filepath.Join(dir, "subdir"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755)) // hidden dir — skipped

	fn := lookup(t, builtins.AddDirectoryListing)

	out, err := fn(t.Context(), &hooks.Input{SessionID: "s", Cwd: dir}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput)
	assert.Equal(t, hooks.EventSessionStart, out.HookSpecificOutput.HookEventName,
		"add_directory_listing must target session_start, not turn_start")

	ctx := out.HookSpecificOutput.AdditionalContext
	assert.Contains(t, ctx, "README.md")
	assert.Contains(t, ctx, "main.go")
	assert.Contains(t, ctx, "subdir/", "directories must be marked with a trailing slash")
	assert.NotContains(t, ctx, ".env", "hidden files must be skipped")
	assert.NotContains(t, ctx, ".git", "hidden directories must be skipped")
}

// TestAddDirectoryListingTruncatesLongList pins the cap behavior: with
// more than maxDirectoryEntries (100) visible entries, the output ends
// with a truncation summary. We assert on the "... and N more" line
// without locking in the exact format of every entry.
func TestAddDirectoryListingTruncatesLongList(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for i := range 150 {
		writeFile(t, dir, fmt.Sprintf("file_%03d.txt", i), "")
	}

	fn := lookup(t, builtins.AddDirectoryListing)

	out, err := fn(t.Context(), &hooks.Input{SessionID: "s", Cwd: dir}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Contains(t, out.HookSpecificOutput.AdditionalContext, "and 50 more",
		"truncation summary must report the dropped count")
}

// TestAddDirectoryListingEmptyOrNoCwdIsNoop documents the two no-op
// paths: an empty Cwd (the safety check) and a Cwd whose only entries
// are hidden (no visible content to report) both produce nil rather
// than an empty stanza.
func TestAddDirectoryListingEmptyOrNoCwdIsNoop(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.AddDirectoryListing)

	out, err := fn(t.Context(), &hooks.Input{SessionID: "s"}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)

	out, err = fn(t.Context(), nil, nil)
	require.NoError(t, err)
	assert.Nil(t, out)

	hiddenOnly := t.TempDir()
	writeFile(t, hiddenOnly, ".hidden", "")
	out, err = fn(t.Context(), &hooks.Input{SessionID: "s", Cwd: hiddenOnly}, nil)
	require.NoError(t, err)
	assert.Nil(t, out, "directory with only hidden entries must yield no output")
}

// TestAddDirectoryListingUnreadableDirIsNoop documents the graceful
// failure path: a Cwd that doesn't exist (or can't be read) makes the
// builtin skip rather than abort the session. ReadDir's error is
// logged at debug level.
func TestAddDirectoryListingUnreadableDirIsNoop(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.AddDirectoryListing)

	out, err := fn(t.Context(), &hooks.Input{
		SessionID: "s",
		Cwd:       filepath.Join(t.TempDir(), "does-not-exist"),
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}
