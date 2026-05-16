package oci

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/content"
	"github.com/docker/docker-agent/pkg/version"
)

// PackageFileAsOCIToStore creates an OCI artifact from a file and stores it in the content store
func PackageFileAsOCIToStore(ctx context.Context, agentSource config.Source, artifactRef string, store *content.Store) (string, error) {
	if !strings.Contains(artifactRef, ":") {
		artifactRef += ":latest"
	}

	cfg, err := config.Load(ctx, agentSource)
	if err != nil {
		return "", fmt.Errorf("loading config: %w", err)
	}

	// Read raw data
	var raw struct {
		Version string `yaml:"version,omitempty"`
	}
	data, err := agentSource.Read(ctx)
	if err != nil {
		return "", fmt.Errorf("reading file: %w", err)
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("looking for version in config file\n%s", yaml.FormatError(err, true, true))
	}

	// Make sure we push a yaml with a version (Use latest if missing)
	if raw.Version == "" {
		cfg.Version = latest.Version
		data, err = yaml.MarshalWithOptions(cfg, yaml.Indent(2))
		if err != nil {
			return "", fmt.Errorf("marshaling config: %w", err)
		}
	}

	// Prepare OCI annotations
	annotations := map[string]string{
		"io.docker.cagent.version":             version.Version,
		"io.docker.agent.version":              version.Version,
		"org.opencontainers.image.created":     time.Now().Format(time.RFC3339),
		"org.opencontainers.image.description": "OCI artifact containing " + filepath.Base(agentSource.Name()),
	}
	if author := cfg.Metadata.Author; author != "" {
		annotations["org.opencontainers.image.authors"] = author
	}
	if license := cfg.Metadata.License; license != "" {
		annotations["org.opencontainers.image.licenses"] = license
	}
	if revision := cfg.Metadata.Version; revision != "" {
		annotations["org.opencontainers.image.revision"] = revision
	}

	layer := static.NewLayer(data, "application/yaml")
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return "", fmt.Errorf("appending layer: %w", err)
	}

	// Convert to OCI manifest format to support annotations
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, "application/vnd.docker.agent.config.v1+json")
	img = mutate.Annotations(img, annotations).(v1.Image)

	digest, err := store.StoreArtifact(img, artifactRef)
	if err != nil {
		return "", fmt.Errorf("storing artifact in content store: %w", err)
	}

	return digest, nil
}
