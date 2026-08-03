package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/docker/docker-agent/pkg/cli/invocation"
	"github.com/docker/docker-agent/pkg/content"
	desktoptransport "github.com/docker/docker-agent/pkg/desktop/transport"
)

// NormalizeReference parses an OCI reference and returns the normalized
// store key that Pull uses to store artifacts. This ensures that equivalent
// references (e.g. "myorg/review-pr" and
// "index.docker.io/myorg/review-pr:latest") map to the same key.
//
// The registry host is deliberately stripped, so references that differ only
// by registry map to the same key. Callers that need a registry-scoped
// identity (e.g. cache keys spanning trust boundaries) must use
// FullyQualifiedReference instead.
func NormalizeReference(registryRef string) (string, error) {
	ref, err := name.ParseReference(registryRef)
	if err != nil {
		return "", fmt.Errorf("parsing registry reference %s: %w", registryRef, err)
	}
	return ref.Context().RepositoryStr() + separator(ref) + ref.Identifier(), nil
}

// FullyQualifiedReference returns the fully-qualified form of an OCI
// reference, including the registry host (e.g.
// "index.docker.io/myorg/review-pr:latest"). Equivalent shorthand
// forms map to the same value, while references that differ only by registry
// map to different values — unlike NormalizeReference, which strips the host.
func FullyQualifiedReference(registryRef string) (string, error) {
	ref, err := name.ParseReference(registryRef)
	if err != nil {
		return "", fmt.Errorf("parsing registry reference %s: %w", registryRef, err)
	}
	return ref.Name(), nil
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

// Pull pulls an artifact from a registry and stores it in the content store.
//
// The digest check, manifest read, and layer downloads all reuse a single
// authenticated registry session (a remote.Puller) so the token exchange and
// underlying connection are established only once, rather than re-doing
// authentication for each step.
func Pull(ctx context.Context, registryRef string, force bool, opts ...crane.Option) (string, error) {
	opts = append(opts, crane.WithContext(ctx), crane.WithTransport(desktoptransport.New(ctx)))
	o := crane.GetOptions(opts...)

	ref, err := name.ParseReference(registryRef, o.Name...)
	if err != nil {
		return "", fmt.Errorf("parsing registry reference %s: %w", registryRef, err)
	}

	s, err := newSession(o)
	if err != nil {
		return "", fmt.Errorf("creating registry session: %w", err)
	}

	remoteDigest, err := s.digest(ctx, ref)
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
					return "", fmt.Errorf("artifact %s found in store wasn't created by `%s share push`\nTry to push again with `%s share push`", localRef, invocation.DockerAgent(), invocation.DockerAgent())
				}
				return meta.Digest, nil
			}
		}
	}

	img, err := s.image(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("pulling image from registry %s: %w", registryRef, err)
	}

	manifest, err := img.Manifest()
	if err != nil {
		return "", fmt.Errorf("getting manifest from pulled image: %w", err)
	}
	if !hasCagentAnnotation(manifest.Annotations) {
		return "", fmt.Errorf("artifact %s wasn't created by `%s share push`\nTry to push again with `%s share push`", localRef, invocation.DockerAgent(), invocation.DockerAgent())
	}

	digest, err := storeArtifact(ctx, store, s, ref, localRef, img)
	if err != nil {
		return "", fmt.Errorf("storing artifact in content store: %w", err)
	}

	return digest, nil
}

func storeArtifact(ctx context.Context, store *content.Store, s *session, ref name.Reference, localRef string, img v1.Image) (string, error) {
	digest, err := store.StoreArtifact(img, localRef)
	if err == nil {
		return digest, nil
	}
	if s.fellBack || !shouldRetryWithCredentials(err) {
		return "", err
	}
	if perr := s.retryWithCredentials(); perr != nil {
		return "", perr
	}

	img, err = s.image(ctx, ref)
	if err != nil {
		return "", err
	}
	manifest, err := img.Manifest()
	if err != nil {
		return "", fmt.Errorf("getting manifest from pulled image: %w", err)
	}
	if !hasCagentAnnotation(manifest.Annotations) {
		return "", fmt.Errorf("artifact %s wasn't created by `%s share push`\nTry to push again with `%s share push`", localRef, invocation.DockerAgent(), invocation.DockerAgent())
	}
	return store.StoreArtifact(img, localRef)
}

// session reuses a single remote.Puller across the digest check, manifest read,
// and layer downloads of a pull. It starts anonymous and transparently falls
// back to credentials from the keychain when the registry denies anonymous
// access or rate-limits the request.
type session struct {
	opts     crane.Options
	puller   *remote.Puller
	fellBack bool
}

func newSession(o crane.Options) (*session, error) {
	s := &session{opts: o}
	puller, err := remote.NewPuller(s.remoteOptions(authn.Anonymous)...)
	if err != nil {
		return nil, err
	}
	s.puller = puller
	return s, nil
}

// digest resolves the remote digest, matching crane.Digest behavior. When a
// platform is set, it resolves indexes to the platform-specific image digest;
// otherwise it uses a cheap HEAD request and falls back to GET if HEAD fails.
func (s *session) digest(ctx context.Context, ref name.Reference) (string, error) {
	if s.opts.Platform != nil {
		d, err := s.get(ctx, ref)
		if err != nil {
			return "", err
		}
		if !d.MediaType.IsIndex() {
			return d.Digest.String(), nil
		}
		img, err := d.Image()
		if err != nil {
			return "", err
		}
		digest, err := img.Digest()
		if err != nil {
			return "", err
		}
		return digest.String(), nil
	}

	var desc *v1.Descriptor
	err := s.withFallback(func(p *remote.Puller) error {
		var headErr error
		desc, headErr = p.Head(ctx, ref)
		return headErr
	})
	if err == nil {
		return desc.Digest.String(), nil
	}

	// HEAD failed (e.g. registry doesn't support it); fall back to GET.
	d, getErr := s.get(ctx, ref)
	if getErr != nil {
		return "", getErr
	}
	return d.Digest.String(), nil
}

// image fetches the manifest and returns a lazy v1.Image whose layer reads
// reuse this session's authenticated fetcher.
func (s *session) image(ctx context.Context, ref name.Reference) (v1.Image, error) {
	desc, err := s.get(ctx, ref)
	if err != nil {
		return nil, err
	}
	return desc.Image()
}

func (s *session) get(ctx context.Context, ref name.Reference) (*remote.Descriptor, error) {
	var desc *remote.Descriptor
	err := s.withFallback(func(p *remote.Puller) error {
		var getErr error
		desc, getErr = p.Get(ctx, ref)
		return getErr
	})
	return desc, err
}

// withFallback runs op against the current puller and, if an anonymous request
// is denied or rate-limited, rebuilds the puller with keychain credentials and
// retries once. Subsequent calls reuse the credentialed puller (and its cached
// token).
func (s *session) withFallback(op func(*remote.Puller) error) error {
	err := op(s.puller)
	if s.fellBack || !shouldRetryWithCredentials(err) {
		return err
	}

	if err := s.retryWithCredentials(); err != nil {
		return err
	}
	return op(s.puller)
}

func (s *session) retryWithCredentials() error {
	if s.fellBack {
		return nil
	}
	puller, err := remote.NewPuller(s.remoteOptions(nil)...)
	if err != nil {
		return err
	}
	s.puller = puller
	s.fellBack = true
	return nil
}

// remoteOptions builds the options for a puller, reusing the exact remote.Option
// slice crane assembled (transport, context, platform, user-agent, jobs, ...).
// crane keeps the authenticator at index 0, so we only override that entry:
// authn.Anonymous for the first attempt, or the keychain (crane's original
// option) for the credentialed fallback.
func (s *session) remoteOptions(auth authn.Authenticator) []remote.Option {
	opts := slices.Clone(s.opts.Remote)
	if auth != nil && len(opts) > 0 {
		opts[0] = remote.WithAuth(auth)
	}
	return opts
}

// shouldRetryWithCredentials reports whether an anonymous registry request
// failed in a way that authenticating might fix: the registry denied access
// (401 Unauthorized or 403 Forbidden) or rate-limited the anonymous request
// (429 Too Many Requests, since authenticated accounts get higher limits).
func shouldRetryWithCredentials(err error) bool {
	var terr *transport.Error
	if !errors.As(err, &terr) {
		return false
	}
	switch terr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
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
