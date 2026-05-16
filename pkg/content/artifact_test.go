package content

import (
	"encoding/json"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewArtifactImage(t *testing.T) {
	t.Parallel()

	const testArtifactType = "application/vnd.test.artifact+json"

	layer := static.NewLayer([]byte("test content"), "application/yaml")
	base, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	base = mutate.MediaType(base, types.OCIManifestSchema1)

	artifact := NewArtifactImage(base, testArtifactType)

	// Manifest must contain artifactType.
	raw, err := artifact.RawManifest()
	require.NoError(t, err)

	var manifest map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &manifest))

	var got string
	require.Contains(t, manifest, "artifactType")
	require.NoError(t, json.Unmarshal(manifest["artifactType"], &got))
	assert.Equal(t, testArtifactType, got)

	// Config must be preserved from the base image (not replaced with {}).
	rawConfig, err := artifact.RawConfigFile()
	require.NoError(t, err)
	baseConfig, err := base.RawConfigFile()
	require.NoError(t, err)
	assert.Equal(t, baseConfig, rawConfig)

	// Layers must still be accessible.
	layers, err := artifact.Layers()
	require.NoError(t, err)
	assert.Len(t, layers, 1)
}
