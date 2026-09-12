package root

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestSetupRecordingProxy_EmptyPath(t *testing.T) {
	t.Parallel()

	var runConfig config.RuntimeConfig

	cassettePath, cleanup, err := setupRecordingProxy(t.Context(), "", &runConfig)

	require.NoError(t, err)
	assert.Empty(t, cassettePath)
	assert.NotNil(t, cleanup)
	assert.Empty(t, runConfig.ModelsGateway, "ModelsGateway should not be set")

	require.NoError(t, cleanup())
}

func TestSetupRecordingProxy_AutoGeneratesFilename(t *testing.T) {
	t.Chdir(t.TempDir())

	var runConfig config.RuntimeConfig

	cassettePath, cleanup, err := setupRecordingProxy(t.Context(), "true", &runConfig)
	require.NoError(t, err)
	defer func() { require.NoError(t, cleanup()) }()

	assert.True(t, strings.HasPrefix(cassettePath, "cagent-recording-"), "should have auto-generated prefix")
	assert.True(t, strings.HasSuffix(cassettePath, ".yaml"), "should have .yaml suffix")
	assert.NotEmpty(t, runConfig.ModelsGateway, "ModelsGateway should be set")
}

func TestSetupRecordingProxy_CreatesProxy(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cassettePath := filepath.Join(tmpDir, "test-recording")

	var runConfig config.RuntimeConfig

	resultPath, cleanup, err := setupRecordingProxy(t.Context(), cassettePath, &runConfig)
	require.NoError(t, err)
	defer func() { require.NoError(t, cleanup()) }()

	assert.Equal(t, cassettePath+".yaml", resultPath)
	assert.True(t, strings.HasPrefix(runConfig.ModelsGateway, "http://"), "ModelsGateway should be HTTP URL")
}

func TestSetupRecordingProxy_NoUpstreamGateway_SuppliesPlaceholderDockerToken(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cassettePath := filepath.Join(tmpDir, "test-recording")

	// No gateway configured: the recording proxy's local address must not
	// require a real Docker Desktop sign-in (issue #4250).
	var runConfig config.RuntimeConfig

	_, cleanup, err := setupRecordingProxy(t.Context(), cassettePath, &runConfig)
	require.NoError(t, err)
	defer func() { require.NoError(t, cleanup()) }()

	require.True(t, environment.IsTrustedDockerURL(runConfig.ModelsGateway), "proxy address should look like a trusted localhost gateway")
	token, ok := runConfig.EnvOverrides[environment.DockerDesktopTokenEnv]
	assert.True(t, ok, "a placeholder Docker token should be supplied")
	assert.NotEmpty(t, token)
}

func TestSetupRecordingProxy_NonDockerUpstreamGateway_SuppliesPlaceholderDockerToken(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cassettePath := filepath.Join(tmpDir, "test-recording")

	// A configured but non-Docker upstream gateway also has no real Docker
	// token to preserve.
	runConfig := config.RuntimeConfig{Config: config.Config{ModelsGateway: "https://example.com/gateway"}}

	_, cleanup, err := setupRecordingProxy(t.Context(), cassettePath, &runConfig)
	require.NoError(t, err)
	defer func() { require.NoError(t, cleanup()) }()

	token, ok := runConfig.EnvOverrides[environment.DockerDesktopTokenEnv]
	assert.True(t, ok, "a placeholder Docker token should be supplied")
	assert.NotEmpty(t, token)
}

func TestSetupRecordingProxy_TrustedDockerUpstreamGateway_KeepsRealToken(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cassettePath := filepath.Join(tmpDir, "test-recording")

	// A genuine upstream Docker gateway needs the real Docker Desktop token
	// to keep flowing through untouched.
	runConfig := config.RuntimeConfig{Config: config.Config{ModelsGateway: "https://my-gateway.docker.com"}}

	_, cleanup, err := setupRecordingProxy(t.Context(), cassettePath, &runConfig)
	require.NoError(t, err)
	defer func() { require.NoError(t, cleanup()) }()

	_, ok := runConfig.EnvOverrides[environment.DockerDesktopTokenEnv]
	assert.False(t, ok, "a real upstream Docker gateway must not get a placeholder token")
}

func TestSetupFakeProxy_NoUpstreamGateway_SuppliesPlaceholderDockerToken(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cassettePath := filepath.Join(tmpDir, "test-fake")
	require.NoError(t, os.WriteFile(cassettePath+".yaml", []byte("version: 2\ninteractions: []\n"), 0o600))

	var runConfig config.RuntimeConfig

	cleanup, err := setupFakeProxy(t.Context(), cassettePath, 0, &runConfig)
	require.NoError(t, err)
	defer func() { require.NoError(t, cleanup()) }()

	token, ok := runConfig.EnvOverrides[environment.DockerDesktopTokenEnv]
	assert.True(t, ok, "a placeholder Docker token should be supplied")
	assert.NotEmpty(t, token)
}
