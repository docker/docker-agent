package oci

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/content"
)

func TestPackageFileAsOCIToStore(t *testing.T) {
	agentFilename := filepath.Join(t.TempDir(), "test.yaml")
	testContent := `version: "2"
agents:
  root:
    model: auto
    description: A helpful AI assistant
`
	require.NoError(t, os.WriteFile(agentFilename, []byte(testContent), 0o644))
	store, err := content.NewStore(content.WithBaseDir(t.TempDir()))
	require.NoError(t, err)

	agentSource, err := config.Resolve(agentFilename, nil)
	require.NoError(t, err)

	tag := "test-app:v1.0.0"
	digest, err := PackageFileAsOCIToStore(t.Context(), agentSource, tag, store)
	require.NoError(t, err)
	assert.NotEmpty(t, digest)
	t.Cleanup(func() {
		if err := store.DeleteArtifact(digest); err != nil {
			t.Logf("Failed to clean up artifact: %v", err)
		}
	})

	img, err := store.GetArtifactImage(tag)
	require.NoError(t, err)
	assert.NotNil(t, img)

	metadata, err := store.GetArtifactMetadata(tag)
	require.NoError(t, err)

	assert.Equal(t, tag, metadata.Reference)
	assert.Equal(t, digest, metadata.Digest)

	// Verify annotations are present
	require.NotNil(t, metadata.Annotations)
	assert.Contains(t, metadata.Annotations, "org.opencontainers.image.created")
	assert.Contains(t, metadata.Annotations, "org.opencontainers.image.description")
	assert.Equal(t, "OCI artifact containing test.yaml", metadata.Annotations["org.opencontainers.image.description"])
}

func TestPackageFileAsOCIToStoreInvalidTag(t *testing.T) {
	agentFilename := filepath.Join(t.TempDir(), "test.txt")
	require.NoError(t, os.WriteFile(agentFilename, []byte("test content"), 0o644))

	agentSource, err := config.Resolve(agentFilename, nil)
	require.NoError(t, err)

	store, err := content.NewStore(content.WithBaseDir(t.TempDir()))
	require.NoError(t, err)
	_, err = PackageFileAsOCIToStore(t.Context(), agentSource, "", store)
	require.Error(t, err)
}

func TestPackageFileAsOCIToStore_WithProviders(t *testing.T) {
	// Test that configs with providers are correctly marshalled when packaged
	// This is important because configs without version get re-marshalled
	agentFilename := filepath.Join(t.TempDir(), "test.yaml")
	testContent := `providers:
  my_gateway:
    api_type: openai_chatcompletions
    base_url: http://localhost:8080
    token_key: MY_API_KEY

agents:
  root:
    model: my_gateway/gpt-4o
    description: Test agent
`
	require.NoError(t, os.WriteFile(agentFilename, []byte(testContent), 0o644))
	store, err := content.NewStore(content.WithBaseDir(t.TempDir()))
	require.NoError(t, err)

	agentSource, err := config.Resolve(agentFilename, nil)
	require.NoError(t, err)

	tag := "test-providers:v1.0.0"
	digest, err := PackageFileAsOCIToStore(t.Context(), agentSource, tag, store)
	require.NoError(t, err)
	assert.NotEmpty(t, digest)

	t.Cleanup(func() {
		_ = store.DeleteArtifact(digest)
	})

	// Pull the artifact and verify providers are preserved
	img, err := store.GetArtifactImage(tag)
	require.NoError(t, err)

	layers, err := img.Layers()
	require.NoError(t, err)
	require.Len(t, layers, 1)

	reader, err := layers[0].Uncompressed()
	require.NoError(t, err)
	defer reader.Close()

	data, err := io.ReadAll(reader)
	require.NoError(t, err)

	// Verify the providers section is present with correct keys
	assert.Contains(t, string(data), "providers:")
	assert.Contains(t, string(data), "my_gateway:")
	assert.Contains(t, string(data), "api_type:")
	assert.Contains(t, string(data), "base_url:")
	assert.Contains(t, string(data), "token_key:")
}
