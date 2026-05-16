package toolinstall

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolsDir_Default(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", "")

	dir := ToolsDir()
	assert.Contains(t, dir, "tools")
}

func TestToolsDir_EnvOverride(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", "/custom/tools/dir")

	dir := ToolsDir()
	assert.Equal(t, "/custom/tools/dir", dir)
}

func TestBinDir(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", "/custom/tools")

	dir := BinDir()
	assert.Equal(t, "/custom/tools/bin", dir)
}

func TestPackageDir(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", "/custom/tools")

	dir := PackageDir("cli", "cli", "v2.50.0")
	expected := "/custom/tools/packages/cli/cli/v2.50.0"
	assert.Equal(t, expected, dir)
}

func TestRegistryDir(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", "/custom/tools")

	dir := RegistryDir()
	assert.Equal(t, "/custom/tools/registry", dir)
}

func TestPrependBinDirToEnv_WithExistingPATH(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", "/custom/tools")

	env := []string{
		"HOME=/home/user",
		"PATH=/usr/bin:/usr/local/bin",
		"FOO=bar",
	}

	result := PrependBinDirToEnv(env)

	require.Len(t, result, 3)
	assert.Equal(t, "HOME=/home/user", result[0])
	assert.Equal(t, "PATH=/custom/tools/bin"+string(os.PathListSeparator)+"/usr/bin:/usr/local/bin", result[1])
	assert.Equal(t, "FOO=bar", result[2])
}

func TestPrependBinDirToEnv_NoPATH(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", "/custom/tools")

	env := []string{
		"HOME=/home/user",
		"FOO=bar",
	}

	result := PrependBinDirToEnv(env)

	require.Len(t, result, 3)
	assert.Equal(t, "HOME=/home/user", result[0])
	assert.Equal(t, "FOO=bar", result[1])
	assert.Equal(t, "PATH=/custom/tools/bin", result[2])
}

func TestPrependBinDirToEnv_EmptyEnv(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", "/custom/tools")

	result := PrependBinDirToEnv(nil)

	require.Len(t, result, 1)
	assert.Equal(t, "PATH=/custom/tools/bin", result[0])
}
