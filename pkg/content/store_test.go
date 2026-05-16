package content

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreBasicOperations(t *testing.T) {
	store, err := NewStore(WithBaseDir(t.TempDir()))
	require.NoError(t, err)

	testData := []byte("Hello, World! This is a test artifact.")
	layer := static.NewLayer(testData, types.OCIUncompressedLayer)
	img := empty.Image
	img, err = mutate.AppendLayers(img, layer)
	require.NoError(t, err)

	testRef := "hello-world:v1.0.0"
	digest, err := store.StoreArtifact(img, testRef)
	require.NoError(t, err)

	retrievedImg, err := store.GetArtifactImage(testRef)
	require.NoError(t, err)
	assert.NotNil(t, retrievedImg)

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

func TestStoreMultipleArtifacts(t *testing.T) {
	store, err := NewStore(WithBaseDir(t.TempDir()))
	require.NoError(t, err)

	testRefs := []string{
		"app1:v1.0.0",
		"app2:v2.0.0",
		"app3:latest",
	}

	for i, ref := range testRefs {
		testData := fmt.Appendf(nil, "Test artifact %d", i+1)
		layer := static.NewLayer(testData, types.OCIUncompressedLayer)
		img := empty.Image
		img, err = mutate.AppendLayers(img, layer)
		require.NoError(t, err)

		digest, err := store.StoreArtifact(img, ref)
		require.NoError(t, err)

		assert.NotEmpty(t, digest)
	}

	artifacts, err := store.ListArtifacts()
	require.NoError(t, err)

	assert.GreaterOrEqual(t, len(artifacts), len(testRefs))

	for _, ref := range testRefs {
		img, err := store.GetArtifactImage(ref)
		require.NoError(t, err)
		assert.NotNil(t, img)
	}
}

func TestStoreResolution(t *testing.T) {
	store, err := NewStore(WithBaseDir(t.TempDir()))
	require.NoError(t, err)

	testData := []byte("Resolution test artifact")
	layer := static.NewLayer(testData, types.OCIUncompressedLayer)
	img := empty.Image
	img, err = mutate.AppendLayers(img, layer)
	require.NoError(t, err)

	testRef := "resolution-test:latest"
	_, err = store.StoreArtifact(img, testRef)
	require.NoError(t, err)

	testCases := []string{
		testRef,
	}

	for _, tc := range testCases {
		img, err := store.GetArtifactImage(tc)
		require.NoError(t, err)
		assert.NotNil(t, img)
	}
}

func TestStoreResolution_DigestReference(t *testing.T) {
	store, err := NewStore(WithBaseDir(t.TempDir()))
	require.NoError(t, err)

	testData := []byte("Digest resolution test")
	layer := static.NewLayer(testData, types.OCIUncompressedLayer)
	img := empty.Image
	img, err = mutate.AppendLayers(img, layer)
	require.NoError(t, err)

	tagRef := "myrepo/agent:v1"
	digest, err := store.StoreArtifact(img, tagRef)
	require.NoError(t, err)

	// Bare digest should resolve.
	retrievedImg, err := store.GetArtifactImage(digest)
	require.NoError(t, err)
	assert.NotNil(t, retrievedImg)

	// Digest reference (repo@sha256:...) should also resolve.
	digestRef := "myrepo/agent@" + digest
	retrievedImg, err = store.GetArtifactImage(digestRef)
	require.NoError(t, err)
	assert.NotNil(t, retrievedImg)

	// Metadata lookup via digest reference should work too.
	meta, err := store.GetArtifactMetadata(digestRef)
	require.NoError(t, err)
	assert.Equal(t, digest, meta.Digest)
}

// TestStoreResolution_RejectsMalformedDigest ensures that identifiers
// shaped like a digest but carrying path-traversal sequences are rejected
// before they can be joined into a filesystem path.
func TestStoreResolution_RejectsMalformedDigest(t *testing.T) {
	baseDir := t.TempDir()
	store, err := NewStore(WithBaseDir(baseDir))
	require.NoError(t, err)

	// Create a sentinel file outside baseDir to confirm we don't touch it.
	sentinelDir := t.TempDir()
	sentinel := filepath.Join(sentinelDir, "sentinel.tar")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep me"), 0o600))

	malformed := []string{
		"sha256:../../etc/passwd",
		"sha256:../" + filepath.Base(sentinelDir) + "/sentinel",
		"sha256:",
		"sha256:deadbeef", // too short
		// non-hex char in an otherwise 64-char body
		"sha256:z0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde",
		"repo@sha256:../../oops",
	}

	for _, id := range malformed {
		_, err := store.GetArtifactImage(id)
		require.ErrorIsf(t, err, ErrInvalidDigest, "id=%q", id)

		_, err = store.GetArtifactPath(id)
		require.ErrorIsf(t, err, ErrInvalidDigest, "id=%q", id)

		err = store.DeleteArtifact(id)
		require.ErrorIsf(t, err, ErrInvalidDigest, "id=%q", id)
	}

	// Sentinel must be untouched.
	_, err = os.Stat(sentinel)
	require.NoError(t, err, "sentinel file should not have been affected")
}
