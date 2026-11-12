package fsx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeAgentsIgnore(t *testing.T, dir, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, AgentsIgnoreFile), []byte(body), 0o644))
}

func TestAgentsIgnoreAbsentIsNilAndInert(t *testing.T) {
	m, err := NewAgentsIgnoreMatcher(t.TempDir())
	require.NoError(t, err)
	require.Nil(t, m)
	assert.False(t, m.Match("/anything"))
	assert.Empty(t, m.Root())
}

func TestAgentsIgnoreWorksWithoutGitRepo(t *testing.T) {
	dir := t.TempDir()
	writeAgentsIgnore(t, dir, "secrets.env\n")

	m, err := NewAgentsIgnoreMatcher(dir)
	require.NoError(t, err)
	require.NotNil(t, m, "a .agentsignore must work outside a git repository")
	assert.True(t, m.Match(filepath.Join(dir, "secrets.env")))
}

func TestAgentsIgnoreGitignoreSyntax(t *testing.T) {
	dir := t.TempDir()
	writeAgentsIgnore(t, dir, `
# comment, ignored
secrets.env
*.key
build/
docs/**/*.draft.md
!keep.key
`)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "build", "out"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "a"), 0o755))

	m, err := NewAgentsIgnoreMatcher(dir)
	require.NoError(t, err)
	require.NotNil(t, m)

	for _, tc := range []struct {
		path string
		want bool
		why  string
	}{
		{"secrets.env", true, "exact name"},
		{"sub/secrets.env", true, "bare name matches at any depth"},
		{"id.key", true, "glob"},
		{"keep.key", false, "negation re-includes"},
		{"build", true, "directory itself"},
		{"build/out", true, "inside an ignored directory"},
		{"docs/a/spec.draft.md", true, "double-star glob"},
		{"docs/a/spec.md", false, "not matched"},
		{"README.md", false, "unrelated file"},
	} {
		got := m.Match(filepath.Join(dir, tc.path))
		assert.Equalf(t, tc.want, got, "%s (%s)", tc.path, tc.why)
	}
}

func TestAgentsIgnoreHidesItself(t *testing.T) {
	dir := t.TempDir()
	writeAgentsIgnore(t, dir, "secrets.env\n")

	m, err := NewAgentsIgnoreMatcher(dir)
	require.NoError(t, err)
	assert.True(t, m.Match(filepath.Join(dir, AgentsIgnoreFile)))
}

func TestAgentsIgnoreFoundFromSubdirectory(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	writeAgentsIgnore(t, root, "secrets.env\n")

	m, err := NewAgentsIgnoreMatcher(sub)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.True(t, m.Match(filepath.Join(root, "secrets.env")),
		"patterns anchor to the file's directory, not the start directory")
}

func TestAgentsIgnoreDoesNotMatchOutsideItsTree(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	writeAgentsIgnore(t, root, "secrets.env\n")

	m, err := NewAgentsIgnoreMatcher(root)
	require.NoError(t, err)
	assert.False(t, m.Match(filepath.Join(other, "secrets.env")))
}

func TestAgentsIgnoreResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	writeAgentsIgnore(t, dir, "secrets.env\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secrets.env"), []byte("k=v"), 0o600))

	link := filepath.Join(dir, "alias.txt")
	if err := os.Symlink(filepath.Join(dir, "secrets.env"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	m, err := NewAgentsIgnoreMatcher(dir)
	require.NoError(t, err)
	assert.True(t, m.Match(link), "a symlink to an ignored file must stay ignored")
}

func TestAgentsIgnoreEmptyYieldsNoMatcher(t *testing.T) {
	dir := t.TempDir()
	writeAgentsIgnore(t, dir, "# just a comment\n\n   \n")

	m, err := NewAgentsIgnoreMatcher(dir)
	require.NoError(t, err)
	assert.Nil(t, m)
}

func TestReadAgentsIgnoreGlobs(t *testing.T) {
	dir := t.TempDir()
	writeAgentsIgnore(t, dir, "# comment\n\nsecrets.env\n  *.key  \n!keep.key\n")

	globs, err := ReadAgentsIgnoreGlobs(filepath.Join(dir, AgentsIgnoreFile))
	require.NoError(t, err)
	assert.Equal(t, []string{"secrets.env", "*.key", "!keep.key"}, globs,
		"comments and blanks are dropped, order and negation preserved")
}

func TestReadAgentsIgnoreGlobsLongLine(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", 100_000)
	writeAgentsIgnore(t, dir, long+"\n# comment\n\n*.log\n")

	globs, err := ReadAgentsIgnoreGlobs(filepath.Join(dir, AgentsIgnoreFile))
	require.NoError(t, err, "an over-long line must not fail the whole load")
	assert.Equal(t, []string{long, "*.log"}, globs)

	patterns, err := readAgentsIgnorePatterns(filepath.Join(dir, AgentsIgnoreFile))
	require.NoError(t, err)
	assert.Len(t, patterns, 2)
}
