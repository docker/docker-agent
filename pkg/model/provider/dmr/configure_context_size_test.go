package dmr

import (
	"math"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestParseContextSizeFromYAMLInteger(t *testing.T) {
	t.Parallel()

	var opts map[string]any
	require.NoError(t, yaml.Unmarshal([]byte("context_size: 64000\n"), &opts))
	_, isUint := opts["context_size"].(uint64)
	require.True(t, isUint, "goccy should decode a bare integer as uint64, got %T", opts["context_size"])

	got := parseContextSize(opts)
	require.NotNil(t, got)
	assert.Equal(t, int64(64000), *got)

	res, err := parseDMRProviderOpts("llama.cpp", &latest.ModelConfig{ProviderOpts: opts})
	require.NoError(t, err)
	require.NotNil(t, res.contextSize)
	assert.Equal(t, int64(64000), *res.contextSize)
}

func TestParseInt64ValueNumericForms(t *testing.T) {
	t.Parallel()

	n, ok := parseInt64Value(uint64(64000))
	require.True(t, ok)
	assert.Equal(t, int64(64000), n)

	_, ok = parseInt64Value(uint64(math.MaxUint64))
	assert.False(t, ok)

	n, ok = parseInt64Value(" 64000 ")
	require.True(t, ok)
	assert.Equal(t, int64(64000), n)

	assert.Nil(t, parseContextSize(map[string]any{}))
	assert.Nil(t, parseContextSize(map[string]any{"context_size": true}))
}
