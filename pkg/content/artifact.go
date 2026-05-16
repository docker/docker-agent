package content

import (
	"encoding/json"
	"fmt"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
)

// artifactImage wraps a v1.Image so its serialized manifest includes an
// "artifactType" field. The underlying config and layers are preserved
// unchanged, which is required for tarball round-trips (the Docker tarball
// format relies on DiffIDs in the config to map layers).
//
// This is necessary because go-containerregistry's v1.Manifest struct does not
// have an artifactType field.
//
// See https://github.com/opencontainers/image-spec/blob/v1.1.1/manifest.md#guidelines-for-artifact-usage
type artifactImage struct {
	v1.Image

	artifactType string
}

// NewArtifactImage wraps an image so its serialized manifest includes the
// given artifactType.
func NewArtifactImage(base v1.Image, artifactType string) v1.Image {
	return &artifactImage{Image: base, artifactType: artifactType}
}

// RawManifest returns the manifest with artifactType injected.
func (a *artifactImage) RawManifest() ([]byte, error) {
	raw, err := a.Image.RawManifest()
	if err != nil {
		return nil, fmt.Errorf("getting raw manifest: %w", err)
	}

	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("unmarshaling manifest: %w", err)
	}

	at, err := json.Marshal(a.artifactType)
	if err != nil {
		return nil, fmt.Errorf("marshaling artifactType: %w", err)
	}
	manifest["artifactType"] = at

	return json.Marshal(manifest)
}

// Digest returns the sha256 of the modified manifest.
func (a *artifactImage) Digest() (v1.Hash, error) { return partial.Digest(a) }

// Manifest parses the modified raw manifest into a v1.Manifest.
func (a *artifactImage) Manifest() (*v1.Manifest, error) { return partial.Manifest(a) }

// Size returns the size of the modified manifest.
func (a *artifactImage) Size() (int64, error) { return partial.Size(a) }
