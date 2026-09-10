package remote

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/docker/docker-agent/pkg/content"
	"github.com/docker/docker-agent/pkg/desktop/transport"
)

// Push pushes an artifact from the content store to an OCI registry
func Push(ctx context.Context, reference string) error {
	store, err := content.NewStore()
	if err != nil {
		return fmt.Errorf("creating content store: %w", err)
	}

	storeKey, err := FullyQualifiedReference(reference)
	if err != nil {
		return err
	}

	img, err := store.GetArtifactImage(storeKey)
	if err != nil {
		return fmt.Errorf("loading artifact from store: %w", err)
	}

	// Get metadata to restore annotations
	metadata, err := store.GetArtifactMetadata(storeKey)
	if err != nil {
		return fmt.Errorf("loading artifact metadata: %w", err)
	}

	// Convert to OCI format and restore annotations if present
	if len(metadata.Annotations) > 0 {
		img = mutate.MediaType(img, types.OCIManifestSchema1)
		img = mutate.Annotations(img, metadata.Annotations).(v1.Image)
	}

	// Wrap as a spec-compliant OCI artifact so the pushed manifest includes
	// artifactType and an empty config descriptor.
	img = content.NewArtifactImage(img, "application/vnd.docker.agent.config.v1+json")

	ref, err := name.ParseReference(reference)
	if err != nil {
		return fmt.Errorf("parsing registry reference %s: %w", reference, err)
	}

	if err := crane.Push(img, ref.String(), crane.WithContext(ctx), crane.WithTransport(transport.New(ctx))); err != nil {
		return fmt.Errorf("pushing image to registry %s: %w", reference, err)
	}

	return nil
}
