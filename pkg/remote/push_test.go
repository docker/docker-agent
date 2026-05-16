package remote

import (
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/content"
)

func TestPush(t *testing.T) {
	store, err := content.NewStore(content.WithBaseDir(t.TempDir()))
	require.NoError(t, err)

	testData := []byte("test artifact data")

	layer := static.NewLayer(testData, types.OCIUncompressedLayer)
	img := empty.Image
	img, err = mutate.AppendLayers(img, layer)
	require.NoError(t, err)

	testRef := "test-app:latest"
	digest, err := store.StoreArtifact(img, testRef)
	require.NoError(t, err)

	t.Cleanup(func() {
		if err := store.DeleteArtifact(digest); err != nil {
			t.Logf("Failed to clean up artifact: %v", err)
		}
	})

	loadedImg, err := store.GetArtifactImage(testRef)
	require.NoError(t, err)
	assert.NotNil(t, loadedImg)

	err = Push(t.Context(), "invalid:reference:with:too:many:colons")
	require.Error(t, err)

	err = Push(t.Context(), "invalid:reference:with:too:many:colons")
	require.Error(t, err)
}

func TestPushNonExistentArtifact(t *testing.T) {
	err := Push(t.Context(), "registry.example.com/test:latest")
	require.Error(t, err)

	err = Push(t.Context(), "registry.example.com/test:latest")
	require.Error(t, err)
}

func TestPushWithOptions(t *testing.T) {
	store, err := content.NewStore(content.WithBaseDir(t.TempDir()))
	require.NoError(t, err)

	testData := []byte("test artifact data with options")

	layer := static.NewLayer(testData, types.OCIUncompressedLayer)
	img := empty.Image
	img, err = mutate.AppendLayers(img, layer)
	require.NoError(t, err)

	testRef := "test-app-options:v1.0.0"
	digest, err := store.StoreArtifact(img, testRef)
	require.NoError(t, err)

	if err := store.DeleteArtifact(digest); err != nil {
		t.Logf("Failed to clean up artifact: %v", err)
	}
}

func TestContentStore(t *testing.T) {
	store, err := content.NewStore(content.WithBaseDir(t.TempDir()))
	require.NoError(t, err)

	testData := []byte("test content store")

	layer := static.NewLayer(testData, types.OCIUncompressedLayer)
	img := empty.Image
	img, err = mutate.AppendLayers(img, layer)
	require.NoError(t, err)

	testRef := "test-store:latest"
	digest, err := store.StoreArtifact(img, testRef)
	require.NoError(t, err)

	t.Cleanup(func() {
		if err := store.DeleteArtifact(digest); err != nil {
			t.Logf("Failed to clean up artifact: %v", err)
		}
	})

	metadata, err := store.GetArtifactMetadata(testRef)
	require.NoError(t, err)

	assert.Equal(t, testRef, metadata.Reference)
	assert.Equal(t, digest, metadata.Digest)

	artifacts, err := store.ListArtifacts()
	require.NoError(t, err)

	found := false
	for _, artifact := range artifacts {
		if artifact.Reference == testRef {
			found = true
			break
		}
	}

	assert.True(t, found, "Artifact not found in list")
}
