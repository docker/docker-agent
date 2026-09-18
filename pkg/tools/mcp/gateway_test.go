package mcp

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/gateway"
)

// mapEnvProvider is a test double for environment.Provider.
type mapEnvProvider map[string]string

func (m mapEnvProvider) Get(_ context.Context, name string) (string, bool) {
	v, ok := m[name]
	return v, ok
}

// redirectTempDir points os.TempDir (and therefore writeTempFile) at a
// per-test directory so tests never touch the real global temp dir.
// Callers must not use t.Parallel because of t.Setenv.
func redirectTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("TMP", dir)
		t.Setenv("TEMP", dir)
	} else {
		t.Setenv("TMPDIR", dir)
	}
	return dir
}

// writeFileWithMtime creates a file and pins its mtime so sweep age checks
// are deterministic.
func writeFileWithMtime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	require.NoError(t, os.Chtimes(path, mtime, mtime))
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	_, err := os.Lstat(path)
	assert.ErrorIs(t, err, fs.ErrNotExist, "%s should have been removed", path)
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	_, err := os.Lstat(path)
	assert.NoError(t, err, "%s should have been preserved", path)
}

func TestSweepStaleGatewayTempFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Whole-second times survive coarse filesystem mtime granularity.
	now := time.Now().Truncate(time.Second)
	stale := now.Add(-staleTempFileAge - time.Hour)
	fresh := now.Add(-time.Hour)

	const currentPID = 1234
	const otherPID = "99999"

	staleOtherSecrets := filepath.Join(dir, secretsFilePrefix+otherPID+"-1111")
	staleOtherConfig := filepath.Join(dir, configFilePrefix+otherPID+"-1111")
	staleOwnSecrets := filepath.Join(dir, secretsFilePrefix+"1234-2222")
	staleOwnConfig := filepath.Join(dir, configFilePrefix+"1234-2222")
	staleLegacySecrets := filepath.Join(dir, secretsFilePrefix+"3333")
	staleLegacyConfig := filepath.Join(dir, configFilePrefix+"3333")
	freshOtherSecrets := filepath.Join(dir, secretsFilePrefix+otherPID+"-4444")
	freshLegacyConfig := filepath.Join(dir, configFilePrefix+"4444")
	unrelated := filepath.Join(dir, "other-file")

	writeFileWithMtime(t, staleOtherSecrets, stale)
	writeFileWithMtime(t, staleOtherConfig, stale)
	writeFileWithMtime(t, staleOwnSecrets, stale)
	writeFileWithMtime(t, staleOwnConfig, stale)
	writeFileWithMtime(t, staleLegacySecrets, stale)
	writeFileWithMtime(t, staleLegacyConfig, stale)
	writeFileWithMtime(t, freshOtherSecrets, fresh)
	writeFileWithMtime(t, freshLegacyConfig, fresh)
	writeFileWithMtime(t, unrelated, stale)

	// A directory with a matching name must survive, contents included.
	matchingDir := filepath.Join(dir, secretsFilePrefix+"5555")
	require.NoError(t, os.Mkdir(matchingDir, 0o700))
	nested := filepath.Join(matchingDir, "nested")
	writeFileWithMtime(t, nested, stale)
	require.NoError(t, os.Chtimes(matchingDir, stale, stale))

	require.NoError(t, sweepStaleGatewayTempFiles(dir, currentPID, now, staleTempFileAge))

	assertGone(t, staleOtherSecrets)
	assertGone(t, staleOtherConfig)
	assertGone(t, staleLegacySecrets)
	assertGone(t, staleLegacyConfig)
	assertExists(t, staleOwnSecrets)
	assertExists(t, staleOwnConfig)
	assertExists(t, freshOtherSecrets)
	assertExists(t, freshLegacyConfig)
	assertExists(t, unrelated)
	assertExists(t, matchingDir)
	assertExists(t, nested)
}

func TestSweepStaleGatewayTempFiles_PreservesLookalikes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	stale := now.Add(-staleTempFileAge - time.Hour)

	lookalikes := []string{
		secretsFilePrefix,                 // empty rest
		secretsFilePrefix + "abc",         // legacy shape, non-digits
		secretsFilePrefix + "12a34",       // legacy shape, mixed
		configFilePrefix + "abc-123",      // non-digit pid part
		configFilePrefix + "123-abc",      // non-digit random part
		secretsFilePrefix + "123-456-789", // extra hyphen segment
		configFilePrefix + "-123",         // empty pid part
	}
	for _, name := range lookalikes {
		writeFileWithMtime(t, filepath.Join(dir, name), stale)
	}

	require.NoError(t, sweepStaleGatewayTempFiles(dir, 4242, now, staleTempFileAge))

	for _, name := range lookalikes {
		assertExists(t, filepath.Join(dir, name))
	}
}

func TestSweepStaleGatewayTempFiles_AgeBoundary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	// A maxAge different from staleTempFileAge proves the parameter is
	// honored rather than hardcoded.
	maxAge := time.Hour

	atCutoff := filepath.Join(dir, secretsFilePrefix+"99999-1")
	justPast := filepath.Join(dir, configFilePrefix+"8888")
	writeFileWithMtime(t, atCutoff, now.Add(-maxAge))
	writeFileWithMtime(t, justPast, now.Add(-maxAge-time.Second))

	require.NoError(t, sweepStaleGatewayTempFiles(dir, 4242, now, maxAge))

	// Exactly maxAge old is not yet stale; one second past is.
	assertExists(t, atCutoff)
	assertGone(t, justPast)
}

func TestSweepStaleGatewayTempFiles_MissingDirIsNoOp(t *testing.T) {
	t.Parallel()
	err := sweepStaleGatewayTempFiles(filepath.Join(t.TempDir(), "does-not-exist"), 4242, time.Now(), staleTempFileAge)
	assert.NoError(t, err)
}

func TestSweepStaleGatewayTempFiles_IgnoresSymlinks(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink test not reliable on Windows")
	}

	dir := t.TempDir()
	targetDir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	stale := now.Add(-staleTempFileAge - time.Hour)

	target := filepath.Join(targetDir, "target")
	writeFileWithMtime(t, target, stale)

	// The link carries a sweepable stale name; only its type protects it.
	link := filepath.Join(dir, secretsFilePrefix+"777")
	require.NoError(t, os.Symlink(target, link))

	require.NoError(t, sweepStaleGatewayTempFiles(dir, 4242, now, staleTempFileAge))

	// Neither the link nor its out-of-dir target may be touched.
	assertExists(t, link)
	assertExists(t, target)
}

func TestWriteTempFile_ContentAndPermissions(t *testing.T) {
	dir := redirectTempDir(t)

	path, err := writeTempFile(secretsFilePrefix+"*", []byte("KEY=value"))
	require.NoError(t, err)

	assert.Equal(t, dir, filepath.Dir(path))
	assert.True(t, strings.HasPrefix(filepath.Base(path), secretsFilePrefix))

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "KEY=value", string(content))

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, fs.FileMode(0o600), info.Mode().Perm())
	}
}

func TestWriteSecretsToFile(t *testing.T) {
	secrets := []gateway.Secret{
		{Name: "github.token", Env: "GITHUB_TOKEN"},
		{Name: "other.token", Env: "OTHER_TOKEN"},
	}

	t.Run("writes name=value lines", func(t *testing.T) {
		dir := redirectTempDir(t)
		env := mapEnvProvider{"GITHUB_TOKEN": "abc", "OTHER_TOKEN": "def"}

		path, err := writeSecretsToFile(t.Context(), "github", secrets, env)
		require.NoError(t, err)
		assert.Equal(t, dir, filepath.Dir(path))

		// The written name must parse as owned by this process, otherwise
		// the sweep could reclaim live files.
		pidPart, ours := parseGatewayTempName(filepath.Base(path))
		assert.True(t, ours)
		assert.Equal(t, strconv.Itoa(os.Getpid()), pidPart)

		content, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "github.token=abc\nother.token=def", string(content))
	})

	t.Run("missing env var", func(t *testing.T) {
		_, err := writeSecretsToFile(t.Context(), "github", secrets, mapEnvProvider{"GITHUB_TOKEN": "abc"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "OTHER_TOKEN")
	})

	t.Run("newline in secret", func(t *testing.T) {
		env := mapEnvProvider{"GITHUB_TOKEN": "a\nb", "OTHER_TOKEN": "def"}
		_, err := writeSecretsToFile(t.Context(), "github", secrets, env)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "newline")
	})
}

func TestNewGatewayToolset_ErrorLeavesNoFiles(t *testing.T) {
	dir := redirectTempDir(t)

	ts, err := NewGatewayToolset(t.Context(), "toolset", "github",
		[]gateway.Secret{{Name: "github.token", Env: "MISSING_VAR"}},
		nil, mapEnvProvider{}, "")
	require.NoError(t, err)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "constructor must not create temp files")

	err = ts.Start(t.Context())
	require.Error(t, err)
	entries, err = os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "failed acquisition must not leave temp files")
}

func TestNewGatewayToolset_ConfigErrorLeavesNoFiles(t *testing.T) {
	dir := redirectTempDir(t)

	_, err := NewGatewayToolset(t.Context(), "toolset", "github",
		[]gateway.Secret{{Name: "github.token", Env: "GITHUB_TOKEN"}},
		make(chan int), // goccy/go-yaml cannot marshal channels
		mapEnvProvider{"GITHUB_TOKEN": "abc"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "marshaling config")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "secrets file must be removed when the config write fails")
}

func TestNewGatewayToolset_DoesNotCreateFilesUntilStart(t *testing.T) {
	dir := redirectTempDir(t)

	ts, err := NewGatewayToolset(t.Context(), "toolset", "github",
		[]gateway.Secret{{Name: "github.token", Env: "GITHUB_TOKEN"}},
		map[string]any{"key": "value"},
		mapEnvProvider{"GITHUB_TOKEN": "abc"}, "")
	require.NoError(t, err)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "constructor must remain side-effect free")

	require.NoError(t, ts.prepare(t.Context()))
	entries, err = os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	require.NoError(t, ts.cleanUp(t.Context()))
	entries, err = os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
	require.NoError(t, ts.cleanUp(t.Context()), "cleanup must be idempotent")
}
