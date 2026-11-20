package root

import (
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverRunAgentArgs(t *testing.T) {
	t.Run("explicit argument wins", func(t *testing.T) {
		t.Chdir(t.TempDir())
		require.NoError(t, os.WriteFile("docker-agent.yaml", []byte("agents: {}\n"), 0o644))

		got, discovered := (&runExecFlags{}).discoverRunAgentArgs([]string{"custom.yaml"})

		assert.Equal(t, []string{"custom.yaml"}, got)
		assert.False(t, discovered)
	})

	t.Run("discovers docker-agent yaml before yml", func(t *testing.T) {
		t.Chdir(t.TempDir())
		require.NoError(t, os.WriteFile("docker-agent.yaml", []byte("agents: {}\n"), 0o644))
		require.NoError(t, os.WriteFile("docker-agent.yml", []byte("agents: {}\n"), 0o644))
		require.NoError(t, os.WriteFile("docker-agent.hcl", []byte("agent \"root\" {}\n"), 0o644))

		got, discovered := (&runExecFlags{}).discoverRunAgentArgs(nil)

		assert.Equal(t, []string{"docker-agent.yaml"}, got)
		assert.True(t, discovered)
	})

	t.Run("discovers docker-agent yml", func(t *testing.T) {
		t.Chdir(t.TempDir())
		require.NoError(t, os.WriteFile("docker-agent.yml", []byte("agents: {}\n"), 0o644))

		got, discovered := (&runExecFlags{}).discoverRunAgentArgs(nil)

		assert.Equal(t, []string{"docker-agent.yml"}, got)
		assert.True(t, discovered)
	})

	t.Run("discovers docker-agent hcl last", func(t *testing.T) {
		t.Chdir(t.TempDir())
		require.NoError(t, os.WriteFile("docker-agent.yml", []byte("agents: {}\n"), 0o644))
		require.NoError(t, os.WriteFile("docker-agent.hcl", []byte("agent \"root\" {}\n"), 0o644))

		got, discovered := (&runExecFlags{}).discoverRunAgentArgs(nil)

		assert.Equal(t, []string{"docker-agent.yml"}, got)
		assert.True(t, discovered)
	})

	t.Run("discovers docker-agent hcl", func(t *testing.T) {
		t.Chdir(t.TempDir())
		require.NoError(t, os.WriteFile("docker-agent.hcl", []byte("agent \"root\" {}\n"), 0o644))

		got, discovered := (&runExecFlags{}).discoverRunAgentArgs(nil)

		assert.Equal(t, []string{"docker-agent.hcl"}, got)
		assert.True(t, discovered)
	})

	t.Run("ignores directories", func(t *testing.T) {
		t.Chdir(t.TempDir())
		require.NoError(t, os.Mkdir("docker-agent.yaml", 0o755))
		require.NoError(t, os.WriteFile("docker-agent.yml", []byte("agents: {}\n"), 0o644))

		got, discovered := (&runExecFlags{}).discoverRunAgentArgs(nil)

		assert.Equal(t, []string{"docker-agent.yml"}, got)
		assert.True(t, discovered)
	})

	t.Run("falls back to built-in default", func(t *testing.T) {
		t.Chdir(t.TempDir())

		got, discovered := (&runExecFlags{}).discoverRunAgentArgs(nil)

		assert.Empty(t, got)
		assert.False(t, discovered)
	})

	t.Run("remote run keeps server-side default", func(t *testing.T) {
		t.Chdir(t.TempDir())
		require.NoError(t, os.WriteFile("docker-agent.yaml", []byte("agents: {}\n"), 0o644))

		got, discovered := (&runExecFlags{remoteAddress: "http://127.0.0.1:8080"}).discoverRunAgentArgs(nil)

		assert.Empty(t, got)
		assert.False(t, discovered)
	})

	t.Run("stat errors shadow later candidates", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("self-referential symlink behavior is platform-specific")
		}
		t.Chdir(t.TempDir())
		require.NoError(t, os.Symlink("docker-agent.yaml", "docker-agent.yaml"))
		require.NoError(t, os.WriteFile("docker-agent.yml", []byte("agents: {}\n"), 0o644))

		got, ok := discoverProjectDefaultAgentFile()

		assert.True(t, ok)
		assert.Equal(t, "docker-agent.yaml", got)
	})
}
