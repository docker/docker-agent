// Package ocisource loads agent configurations from OCI artifacts. It is kept
// out of pkg/config so embedders that never read agents from a registry do not
// link the OCI client stack.
package ocisource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/content"
	"github.com/docker/docker-agent/pkg/memoize"
	"github.com/docker/docker-agent/pkg/protect"
	"github.com/docker/docker-agent/pkg/remote"
)

// Option customizes an OCI source.
type Option func(*ociSource)

// WithVerificationKey makes the source verify the artifact protection
// (signature or encrypted copy recorded by `share push --key`) against key on
// every read. Unprotected or tampered artifacts fail to load.
func WithVerificationKey(key *protect.Key) Option {
	return func(o *ociSource) { o.verifyKey = key }
}

// ociSource loads an agent configuration from an OCI artifact.
type ociSource struct {
	reference string
	verifyKey *protect.Key
}

// New returns a source reading the agent configuration stored in the OCI
// artifact at reference. The registry stays the source of truth; the local
// content store is a cache and offline fallback.
func New(reference string, opts ...Option) config.Source {
	s := ociSource{reference: reference}
	for _, opt := range opts {
		opt(&s)
	}
	return s
}

func (a ociSource) Name() string {
	return a.reference
}

func (a ociSource) ParentDir() string {
	return ""
}

// ociReadMemoizer caches successful OCI reads for a short window, keyed by
// fully-qualified reference (registry host included, so references that
// differ only by registry can never share an entry). A single command
// invocation reads the same source several times (the sandbox-default probe
// in runRunCommand, then the team load), and without memoization each read
// pays a blocking registry digest round-trip even when the artifact is
// cached and unchanged. Failed reads are never cached (memoize retries
// them), and neither are degraded reads that fell back to the local store
// because the registry was unreachable — so recovery is retried on the very
// next read. The TTL bounds staleness for long-lived embedders so a tag
// update is picked up within a minute.
var ociReadMemoizer = memoize.New[[]byte](1 * time.Minute)

// pullOCIArtifact pulls (or digest-checks) an OCI artifact from the registry.
// It is a var so tests can stub the network out.
var pullOCIArtifact = func(ctx context.Context, ref string, force bool) (string, error) {
	return remote.Pull(ctx, ref, force)
}

// degradedReadError smuggles a successful-but-degraded read (registry
// unreachable, stale local copy served) out of the memoized closure as an
// error, so memoize does not cache it. Read unwraps it back into a success.
type degradedReadError struct {
	data []byte
}

func (e *degradedReadError) Error() string {
	return "degraded OCI read served from local store"
}

// Read loads an agent configuration from an OCI artifact.
//
// The OCI registry remains the source of truth.
// The local content store is used as a cache and fallback only.
// A forced re-pull is triggered exclusively when store corruption is detected.
//
// Reads validated against the registry are memoized per process (see
// ociReadMemoizer) so repeated loads of the same reference within one command
// don't repeat the registry round-trip. Concurrent readers of the same
// reference share a single in-flight read — including its context: if the
// first caller is cancelled mid-read, waiters get that error too, but since
// errors are never cached the next read simply retries.
func (a ociSource) Read(ctx context.Context) ([]byte, error) {
	// The memoization key must include the registry host: two references that
	// differ only by registry are distinct trust boundaries and must never
	// share cached bytes.
	cacheKey, err := remote.FullyQualifiedReference(a.reference)
	if err != nil {
		return nil, fmt.Errorf("normalizing OCI reference %s: %w", a.reference, err)
	}
	// A verified read must never be served to a reader holding a different
	// key (or none, or only the public half), so the key identity is part of
	// the cache key.
	if a.verifyKey != nil {
		cacheKey += "#" + a.verifyKey.Identity()
	}

	data, err := ociReadMemoizer.Memoize(cacheKey, func() ([]byte, error) {
		data, degraded, err := a.read(ctx)
		if err != nil {
			return nil, err
		}
		if degraded {
			return nil, &degradedReadError{data: data}
		}
		return data, nil
	})
	if degraded, ok := errors.AsType[*degradedReadError](err); ok {
		return degraded.data, nil
	}
	if err != nil {
		return nil, err
	}
	// Clone so no caller can mutate the cached bytes shared with later reads.
	return bytes.Clone(data), nil
}

// read performs the actual (unmemoized) load of the OCI artifact. degraded
// reports that the returned data came from the local store because the
// registry could not be reached — a success for the caller, but one that
// must not be cached so the next read retries the registry.
func (a ociSource) read(ctx context.Context) (data []byte, degraded bool, err error) {
	store, err := content.NewStore()
	if err != nil {
		return nil, false, fmt.Errorf("failed to create content store: %w", err)
	}

	// Normalize the reference so equivalent shorthand forms resolve to the
	// same registry-scoped store key that remote.Pull uses.
	storeKey, err := remote.FullyQualifiedReference(a.reference)
	if err != nil {
		return nil, false, fmt.Errorf("normalizing OCI reference %s: %w", a.reference, err)
	}

	// For digest references, the content is immutable. If we already have
	// the artifact locally, serve it directly without any network call.
	if remote.IsDigestReference(a.reference) {
		if data, loadErr := a.loadArtifact(store, storeKey); loadErr == nil {
			slog.DebugContext(ctx, "Serving digest-pinned OCI artifact from cache", "ref", a.reference)
			return data, false, nil
		}
	}

	// Check whether we have a local copy to fall back on.
	hasLocal := hasLocalArtifact(store, storeKey)

	// Pull from registry (checks remote digest, skips download if unchanged).
	if _, pullErr := pullOCIArtifact(ctx, a.reference, false); pullErr != nil {
		if !hasLocal {
			return nil, false, fmt.Errorf("failed to pull OCI image %s: %w", a.reference, pullErr)
		}
		slog.DebugContext(ctx, "Failed to check for OCI reference updates, using cached version",
			"ref", a.reference, "error", pullErr)
		degraded = true
	}

	// Try loading from store.
	data, err = a.loadArtifact(store, storeKey)
	if err == nil {
		return data, degraded, nil
	}

	// If corrupted, force re-pull and try once more.
	if !errors.Is(err, content.ErrStoreCorrupted) {
		return nil, false, fmt.Errorf("failed to load agent from OCI source %s: %w", a.reference, err)
	}

	slog.WarnContext(ctx, "Local OCI store corrupted, forcing re-pull", "ref", a.reference)
	if _, pullErr := pullOCIArtifact(ctx, a.reference, true); pullErr != nil {
		return nil, false, fmt.Errorf("failed to force re-pull OCI image %s: %w", a.reference, pullErr)
	}

	data, err = a.loadArtifact(store, storeKey)
	if err != nil {
		return nil, false, fmt.Errorf("failed to load agent from OCI source %s: %w", a.reference, err)
	}
	return data, false, nil
}

// loadArtifact reads the agent YAML from the content store and, when a
// verification key is set, checks it against the signature annotations.
func (a ociSource) loadArtifact(store *content.Store, storeKey string) ([]byte, error) {
	af, err := store.GetArtifact(storeKey)
	if err != nil {
		return nil, err
	}
	data := []byte(af)
	if a.verifyKey == nil {
		return data, nil
	}

	meta, err := store.GetArtifactMetadata(storeKey)
	if err != nil {
		return nil, err
	}
	if _, err := a.verifyKey.VerifyAnnotations(meta.Annotations, data); err != nil {
		return nil, fmt.Errorf("verifying %s: %w", a.reference, err)
	}
	return data, nil
}

// hasLocalArtifact reports whether the content store has metadata for the given key.
func hasLocalArtifact(store *content.Store, storeKey string) bool {
	_, err := store.GetArtifactMetadata(storeKey)
	return err == nil
}
