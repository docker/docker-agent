package oci

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/content"
	"github.com/docker/docker-agent/pkg/protect"
)

func TestPackageFileAsOCIToStore(t *testing.T) {
	t.Parallel()
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

	agentSource, err := sources.Resolve(agentFilename, nil)
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

func TestPackageFileAsOCIToStore_MetadataTagsAnnotation(t *testing.T) {
	t.Parallel()
	agentFilename := filepath.Join(t.TempDir(), "test.yaml")
	testContent := `version: "11"
metadata:
  tags:
    - coding
    - review
agents:
  root:
    model: auto
    description: A helpful AI assistant
`
	require.NoError(t, os.WriteFile(agentFilename, []byte(testContent), 0o644))
	store, err := content.NewStore(content.WithBaseDir(t.TempDir()))
	require.NoError(t, err)

	agentSource, err := sources.Resolve(agentFilename, nil)
	require.NoError(t, err)

	tag := "test-tags:v1.0.0"
	digest, err := PackageFileAsOCIToStore(t.Context(), agentSource, tag, store)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.DeleteArtifact(digest) })

	metadata, err := store.GetArtifactMetadata(tag)
	require.NoError(t, err)
	require.NotNil(t, metadata.Annotations)
	assert.Equal(t, "coding,review", metadata.Annotations["io.docker.agent.tags"])
}

func TestPackageFileAsOCIToStore_InlinesInstructionFile(t *testing.T) {
	t.Parallel()
	// A config that uses instruction_file must be pushed self-contained: the
	// file contents are inlined and the now-unresolvable reference is dropped,
	// even when the config carries an explicit version (which normally makes
	// the packager preserve the raw bytes).
	dir := t.TempDir()
	agentFilename := filepath.Join(dir, "test.yaml")
	testContent := `version: "11"
agents:
  root:
    model: auto
    description: Test agent
    instruction_file: instructions/root.md
`
	require.NoError(t, os.WriteFile(agentFilename, []byte(testContent), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "instructions"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "instructions", "root.md"), []byte("You are a self-contained agent."), 0o644))

	store, err := content.NewStore(content.WithBaseDir(t.TempDir()))
	require.NoError(t, err)

	agentSource, err := sources.Resolve(agentFilename, nil)
	require.NoError(t, err)

	tag := "test-instruction-file:v1.0.0"
	digest, err := PackageFileAsOCIToStore(t.Context(), agentSource, tag, store)
	require.NoError(t, err)
	assert.NotEmpty(t, digest)
	t.Cleanup(func() { _ = store.DeleteArtifact(digest) })

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

	// The instruction is inlined and the file reference is gone.
	assert.Contains(t, string(data), "You are a self-contained agent.")
	assert.NotContains(t, string(data), "instruction_file")

	// The pushed artifact loads without the original local file present.
	cfg, err := config.Load(t.Context(), config.NewBytesSource("pulled.yaml", data))
	require.NoError(t, err)
	assert.Equal(t, "You are a self-contained agent.", cfg.Agents.First().Instruction)
}

func TestPackageFileAsOCIToStore_HCLInlinesLocalFiles(t *testing.T) {
	t.Parallel()
	// HCL configs resolve both instruction_file and file() against the local
	// directory, so they are always pushed as the resolved YAML.
	dir := t.TempDir()
	agentFilename := filepath.Join(dir, "agent.hcl")
	testContent := `agent "root" {
  model            = "auto"
  description      = "Root agent"
  instruction_file = "prompts/root.md"
  sub_agents       = ["helper"]
}

agent "helper" {
  model       = "auto"
  description = "Helper agent"
  instruction = file("prompts/helper.md")
}
`
	require.NoError(t, os.WriteFile(agentFilename, []byte(testContent), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "prompts"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompts", "root.md"), []byte("You are the root."), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompts", "helper.md"), []byte("You are the helper."), 0o644))

	store, err := content.NewStore(content.WithBaseDir(t.TempDir()))
	require.NoError(t, err)

	agentSource, err := sources.Resolve(agentFilename, nil)
	require.NoError(t, err)

	tag := "test-hcl-instruction-file:v1.0.0"
	digest, err := PackageFileAsOCIToStore(t.Context(), agentSource, tag, store)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.DeleteArtifact(digest) })

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

	assert.Contains(t, string(data), "You are the root.")
	assert.Contains(t, string(data), "You are the helper.")
	assert.NotContains(t, string(data), "instruction_file")
	assert.NotContains(t, string(data), "file(")

	// The pushed artifact is YAML that loads without the original directory.
	cfg, err := config.Load(t.Context(), config.NewBytesSource("pulled.yaml", data))
	require.NoError(t, err)
	assert.Equal(t, "You are the root.", cfg.Agents.First().Instruction)
}

func TestPackageFileAsOCIToStoreInvalidTag(t *testing.T) {
	t.Parallel()
	agentFilename := filepath.Join(t.TempDir(), "test.txt")
	require.NoError(t, os.WriteFile(agentFilename, []byte("test content"), 0o644))

	agentSource, err := sources.Resolve(agentFilename, nil)
	require.NoError(t, err)

	store, err := content.NewStore(content.WithBaseDir(t.TempDir()))
	require.NoError(t, err)
	_, err = PackageFileAsOCIToStore(t.Context(), agentSource, "", store)
	require.Error(t, err)
}

func TestPackageFileAsOCIToStore_WithProviders(t *testing.T) {
	t.Parallel()
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

	agentSource, err := sources.Resolve(agentFilename, nil)
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

func TestPackageFileAsOCIToStore_WithProtection(t *testing.T) {
	t.Parallel()

	key, err := protect.ParseKey([]byte("a shared secret long enough"))
	require.NoError(t, err)
	other, err := protect.ParseKey([]byte("an other secret long enough"))
	require.NoError(t, err)

	for _, mode := range []protect.Mode{protect.ModeSign, protect.ModeEncrypt} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			agentFilename := filepath.Join(t.TempDir(), "protected.yaml")
			testContent := `version: "2"
agents:
  root:
    model: auto
    description: A protected assistant
`
			require.NoError(t, os.WriteFile(agentFilename, []byte(testContent), 0o644))
			store, err := content.NewStore(content.WithBaseDir(t.TempDir()))
			require.NoError(t, err)

			agentSource, err := sources.Resolve(agentFilename, nil)
			require.NoError(t, err)

			tag := "test-protected:" + string(mode)
			digest, err := PackageFileAsOCIToStore(t.Context(), agentSource, tag, store, WithProtection(key, mode))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.DeleteArtifact(digest) })

			metadata, err := store.GetArtifactMetadata(tag)
			require.NoError(t, err)
			assert.True(t, protect.IsProtected(metadata.Annotations))

			// The protection covers the exact bytes stored in the layer.
			yamlData, err := store.GetArtifact(tag)
			require.NoError(t, err)
			_, err = key.VerifyAnnotations(metadata.Annotations, []byte(yamlData))
			require.NoError(t, err)
			_, err = other.VerifyAnnotations(metadata.Annotations, []byte(yamlData))
			require.Error(t, err)

			if mode == protect.ModeEncrypt {
				// The clear YAML is recoverable, byte-for-byte, from the annotations alone.
				recovered, err := key.Recover(metadata.Annotations)
				require.NoError(t, err)
				assert.True(t, bytes.Equal([]byte(yamlData), recovered))
			}
		})
	}
}

func TestPackageFileAsOCIToStore_WithoutProtectionHasNoAnnotations(t *testing.T) {
	t.Parallel()
	agentFilename := filepath.Join(t.TempDir(), "unsigned.yaml")
	require.NoError(t, os.WriteFile(agentFilename, []byte("version: \"2\"\nagents:\n  root:\n    model: auto\n"), 0o644))
	store, err := content.NewStore(content.WithBaseDir(t.TempDir()))
	require.NoError(t, err)

	agentSource, err := sources.Resolve(agentFilename, nil)
	require.NoError(t, err)

	tag := "test-unsigned:v1"
	digest, err := PackageFileAsOCIToStore(t.Context(), agentSource, tag, store)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.DeleteArtifact(digest) })

	metadata, err := store.GetArtifactMetadata(tag)
	require.NoError(t, err)
	assert.False(t, protect.IsProtected(metadata.Annotations))
}
