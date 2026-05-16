package root

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
)

func TestSetupRecordingProxy_EmptyPath(t *testing.T) {
	var runConfig config.RuntimeConfig

	cassettePath, cleanup, err := setupRecordingProxy("", &runConfig)

	require.NoError(t, err)
	assert.Empty(t, cassettePath)
	assert.NotNil(t, cleanup)
	assert.Empty(t, runConfig.ModelsGateway, "ModelsGateway should not be set")

	require.NoError(t, cleanup())
}

func TestSetupRecordingProxy_AutoGeneratesFilename(t *testing.T) {
	t.Chdir(t.TempDir())

	var runConfig config.RuntimeConfig

	cassettePath, cleanup, err := setupRecordingProxy("true", &runConfig)
	require.NoError(t, err)
	defer func() { require.NoError(t, cleanup()) }()

	assert.True(t, strings.HasPrefix(cassettePath, "cagent-recording-"), "should have auto-generated prefix")
	assert.True(t, strings.HasSuffix(cassettePath, ".yaml"), "should have .yaml suffix")
	assert.NotEmpty(t, runConfig.ModelsGateway, "ModelsGateway should be set")
}

func TestSetupRecordingProxy_CreatesProxy(t *testing.T) {
	tmpDir := t.TempDir()
	cassettePath := filepath.Join(tmpDir, "test-recording")

	var runConfig config.RuntimeConfig

	resultPath, cleanup, err := setupRecordingProxy(cassettePath, &runConfig)
	require.NoError(t, err)
	defer func() { require.NoError(t, cleanup()) }()

	assert.Equal(t, cassettePath+".yaml", resultPath)
	assert.True(t, strings.HasPrefix(runConfig.ModelsGateway, "http://"), "ModelsGateway should be HTTP URL")
}
