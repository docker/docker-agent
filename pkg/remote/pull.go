package remote

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"

	"github.com/docker/docker-agent/pkg/content"
)

// NormalizeReference parses an OCI reference and returns the normalized
// store key that Pull uses to store artifacts. This ensures that equivalent
// references (e.g. "agentcatalog/review-pr" and
// "index.docker.io/agentcatalog/review-pr:latest") map to the same key.
func NormalizeReference(registryRef string) (string, error) {
	ref, err := name.ParseReference(registryRef)
	if err != nil {
		return "", fmt.Errorf("parsing registry reference %s: %w", registryRef, err)
	}
	return ref.Context().RepositoryStr() + separator(ref) + ref.Identifier(), nil
}

// IsDigestReference reports whether the given reference pins a specific
// image digest (e.g. "repo@sha256:abc...").
func IsDigestReference(registryRef string) bool {
	ref, err := name.ParseReference(registryRef)
	if err != nil {
		return false
	}
	_, ok := ref.(name.Digest)
	return ok
}

// Pull pulls an artifact from a registry and stores it in the content store
func Pull(ctx context.Context, registryRef string, force bool, opts ...crane.Option) (string, error) {
	opts = append(opts, crane.WithContext(ctx), crane.WithTransport(NewTransport(ctx)))

	ref, err := name.ParseReference(registryRef)
	if err != nil {
		return "", fmt.Errorf("parsing registry reference %s: %w", registryRef, err)
	}

	remoteDigest, err := crane.Digest(ref.String(), opts...)
	if err != nil {
		return "", fmt.Errorf("resolving remote digest for %s: %w", registryRef, err)
	}

	store, err := content.NewStore()
	if err != nil {
		return "", fmt.Errorf("creating content store: %w", err)
	}

	localRef := ref.Context().RepositoryStr() + separator(ref) + ref.Identifier()
	if !force {
		if meta, metaErr := store.GetArtifactMetadata(localRef); metaErr == nil {
			if meta.Digest == remoteDigest {
				if !hasCagentAnnotation(meta.Annotations) {
					return "", fmt.Errorf("artifact %s found in store wasn't created by `docker agent share push`\nTry to push again with `docker agent share push`", localRef)
				}
				return meta.Digest, nil
			}
		}
	}

	img, err := crane.Pull(ref.String(), opts...)
	if err != nil {
		return "", fmt.Errorf("pulling image from registry %s: %w", registryRef, err)
	}

	manifest, err := img.Manifest()
	if err != nil {
		return "", fmt.Errorf("getting manifest from pulled image: %w", err)
	}
	if !hasCagentAnnotation(manifest.Annotations) {
		return "", fmt.Errorf("artifact %s wasn't created by `docker agent share push`\nTry to push again with `docker agent share push`", localRef)
	}

	digest, err := store.StoreArtifact(img, localRef)
	if err != nil {
		return "", fmt.Errorf("storing artifact in content store: %w", err)
	}

	return digest, nil
}

func hasCagentAnnotation(annotations map[string]string) bool {
	_, exists := annotations["io.docker.agent.version"]
	if !exists {
		_, exists = annotations["io.docker.cagent.version"]
	}
	return exists
}

// separator returns the separator used between repository and identifier.
// For digests it returns "@", for tags it returns ":".
func separator(ref name.Reference) string {
	if _, ok := ref.(name.Digest); ok {
		return "@"
	}
	return ":"
}
