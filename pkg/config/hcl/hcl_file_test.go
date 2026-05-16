package hcl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToYAML_FileFunction(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	instructionsPath := filepath.Join(dir, "instructions.txt")
	require.NoError(t, os.WriteFile(instructionsPath, []byte("Line 1\nLine 2\n"), 0o644))

	src := []byte(`
agent "root" {
  instruction = file("instructions.txt")
  model       = "auto"
}
`)

	m, err := ToMap(src, filepath.Join(dir, "agent.hcl"))
	require.NoError(t, err)

	items := m["agents"].(yaml.MapSlice)
	root := items[0].Value.(map[string]any)
	assert.Equal(t, "Line 1\nLine 2\n", root["instruction"])
}

func TestToYAML_FileFunctionMissingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := []byte(`
agent "root" {
  instruction = file("missing.txt")
  model       = "auto"
}
`)

	_, err := ToMap(src, filepath.Join(dir, "agent.hcl"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading file")
	assert.Contains(t, err.Error(), "missing.txt")
}

func TestToYAML_FileFunctionRejectsTraversal(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	dir := filepath.Join(parent, "config")
	require.NoError(t, os.Mkdir(dir, 0o755))
	secret := filepath.Join(parent, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("nope"), 0o644))

	src := []byte(`
agent "root" {
  instruction = file("../secret.txt")
  model       = "auto"
}
`)

	_, err := ToMap(src, filepath.Join(dir, "agent.hcl"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading file")
	assert.Contains(t, err.Error(), "../secret.txt")
	assert.Contains(t, err.Error(), "local relative path")
}

func TestToYAML_FileFunctionRejectsSymlinkEscape(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	dir := filepath.Join(parent, "config")
	require.NoError(t, os.Mkdir(dir, 0o755))

	outside := filepath.Join(parent, "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o644))

	link := filepath.Join(dir, "instructions.txt")
	if err := os.Symlink("../outside.txt", link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	src := []byte(`
agent "root" {
  instruction = file("instructions.txt")
  model       = "auto"
}
`)

	_, err := ToMap(src, filepath.Join(dir, "agent.hcl"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading file")
	assert.Contains(t, err.Error(), filepath.Join(dir, "instructions.txt"))
}
