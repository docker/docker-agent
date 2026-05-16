package teamloader

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestBuildAgentCache_disabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  *latest.CacheConfig
	}{
		{"nil config", nil},
		{"explicitly disabled", &latest.CacheConfig{Enabled: false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := buildAgentCache("agent", tc.cfg, t.TempDir())
			require.NoError(t, err)
			assert.Nil(t, c)
		})
	}
}

func TestBuildAgentCache_inMemory(t *testing.T) {
	c, err := buildAgentCache("agent", &latest.CacheConfig{Enabled: true}, t.TempDir())
	require.NoError(t, err)
	require.NotNil(t, c)
}

func TestBuildAgentCache_relativePath(t *testing.T) {
	parent := t.TempDir()
	c, err := buildAgentCache("agent",
		&latest.CacheConfig{Enabled: true, Path: "cache.json"}, parent)
	require.NoError(t, err)
	require.NotNil(t, c)

	// Storing must persist to <parent>/cache.json
	c.Store("q", "a")
	_, err = filepath.Abs(filepath.Join(parent, "cache.json"))
	require.NoError(t, err)
}

func TestBuildAgentCache_absolutePath(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "absolute.json")
	c, err := buildAgentCache("agent",
		&latest.CacheConfig{Enabled: true, Path: abs}, t.TempDir())
	require.NoError(t, err)
	require.NotNil(t, c)
}

func TestBuildAgentCache_pathTraversalRejected(t *testing.T) {
	parent := t.TempDir()
	_, err := buildAgentCache("agent",
		&latest.CacheConfig{Enabled: true, Path: "../escape.json"}, parent)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `agent "agent"`)
	assert.Contains(t, err.Error(), "escapes parent directory")
}
