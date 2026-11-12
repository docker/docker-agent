package remote

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	testregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	v1remote "github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/content"
)

type staticKeychain struct {
	auth authn.Authenticator
}

func (k staticKeychain) Resolve(authn.Resource) (authn.Authenticator, error) {
	return k.auth, nil
}

func TestShouldRetryWithCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"nil error", nil, false},
		{"generic error", errors.New("boom"), false},
		{"unauthorized", &transport.Error{StatusCode: http.StatusUnauthorized}, true},
		{"forbidden", &transport.Error{StatusCode: http.StatusForbidden}, true},
		{"too many requests", &transport.Error{StatusCode: http.StatusTooManyRequests}, true},
		{"not found", &transport.Error{StatusCode: http.StatusNotFound}, false},
		{"server error", &transport.Error{StatusCode: http.StatusInternalServerError}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, shouldRetryWithCredentials(tt.err))
		})
	}
}

// testRegistry is a minimal in-memory OCI registry for exercising session
// behavior. It serves one image's manifest and layers and can be configured to
// reject anonymous requests, forcing a credential fallback.
type testRegistry struct {
	img        v1.Image
	manifest   []byte
	mediaType  types.MediaType
	digest     string
	anonReject int // status returned to anonymous manifest/blob requests; 0 allows anonymous
	user, pass string

	pingHits atomic.Int64 // requests to /v2/
	anonHits atomic.Int64 // anonymous manifest/blob requests served
	authHits atomic.Int64 // credentialed manifest/blob requests served
	reqHits  atomic.Int64 // all HTTP requests received
}

func newTestRegistry(t *testing.T) *testRegistry {
	t.Helper()

	layer := static.NewLayer([]byte("test layer"), types.OCIUncompressedLayer)
	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	img = mutate.Annotations(img, map[string]string{"io.docker.agent.version": "test"}).(v1.Image)
	manifest, err := img.RawManifest()
	require.NoError(t, err)
	dig, err := img.Digest()
	require.NoError(t, err)
	mt, err := img.MediaType()
	require.NoError(t, err)

	return &testRegistry{
		img:       img,
		manifest:  manifest,
		mediaType: mt,
		digest:    dig.String(),
	}
}

func (reg *testRegistry) start(t *testing.T) *httptest.Server {
	t.Helper()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reg.reqHits.Add(1)
		if r.URL.Path == "/v2/" {
			reg.pingHits.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}

		rejectStatus := reg.anonReject

		_, _, hasAuth := r.BasicAuth()
		if rejectStatus != 0 {
			if !hasAuth {
				reg.anonHits.Add(1)
				if rejectStatus == http.StatusUnauthorized {
					w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
				}
				w.WriteHeader(rejectStatus)
				return
			}
			u, p, _ := r.BasicAuth()
			if u != reg.user || p != reg.pass {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			reg.authHits.Add(1)
		} else {
			reg.anonHits.Add(1)
		}

		switch {
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Docker-Content-Digest", reg.digest)
			w.Header().Set("Content-Type", string(reg.mediaType))
			if r.Method == http.MethodGet {
				_, _ = w.Write(reg.manifest)
			}
		case strings.Contains(r.URL.Path, "/blobs/"):
			layers, _ := reg.img.Layers()
			if len(layers) == 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			rc, err := layers[0].Compressed()
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			defer rc.Close()
			_, _ = io.Copy(w, rc)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func newTestSession(t *testing.T, opts ...crane.Option) *session {
	t.Helper()
	opts = append(opts, crane.Insecure)
	o := crane.GetOptions(opts...)
	// Keep the default retry count but drop the default backoff (1s+3s
	// sleeps): the 429 rate-limit test otherwise idles for ~4s.
	o.Remote = append(o.Remote, v1remote.WithRetryBackoff(v1remote.Backoff{
		Duration: time.Millisecond,
		Factor:   1,
		Steps:    3,
	}))
	s, err := newSession(o)
	require.NoError(t, err)
	return s
}

func TestSessionDigestAnonymousFirst(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	server := reg.start(t)

	registry := strings.TrimPrefix(server.URL, "http://")
	ref, err := name.ParseReference(registry + "/anon:latest")
	require.NoError(t, err)

	// A keychain that would fail if used, ensuring anonymous access is preferred.
	keychain := staticKeychain{auth: &authn.Basic{Username: "bad", Password: "creds"}}
	s := newTestSession(t, crane.WithAuthFromKeychain(keychain))

	dig, err := s.digest(t.Context(), ref)
	require.NoError(t, err)
	assert.Equal(t, reg.digest, dig)
	assert.Positive(t, reg.anonHits.Load(), "expected an anonymous request")
	assert.Zero(t, reg.authHits.Load(), "did not expect a credentialed request")
}

func TestSessionCredentialFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		anonReject int
	}{
		{"unauthorized", http.StatusUnauthorized},
		{"rate limited", http.StatusTooManyRequests},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reg := newTestRegistry(t)
			reg.anonReject = tt.anonReject
			reg.user, reg.pass = "user", "pass"
			server := reg.start(t)

			registry := strings.TrimPrefix(server.URL, "http://")
			ref, err := name.ParseReference(registry + "/private:latest")
			require.NoError(t, err)

			keychain := staticKeychain{auth: &authn.Basic{Username: "user", Password: "pass"}}
			s := newTestSession(t, crane.WithAuthFromKeychain(keychain))

			dig, err := s.digest(t.Context(), ref)
			require.NoError(t, err)
			assert.Equal(t, reg.digest, dig)
			assert.Positive(t, reg.anonHits.Load(), "expected an initial anonymous attempt")
			assert.Positive(t, reg.authHits.Load(), "expected a credentialed retry")
		})
	}
}

func TestStoreArtifactCredentialFallbackOnLayerDownload(t *testing.T) {
	t.Parallel()

	var requireBlobAuth atomic.Bool
	var anonHits atomic.Int64
	var authHits atomic.Int64
	registryHandler := testregistry.New(testregistry.Logger(log.New(io.Discard, "", 0)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requireBlobAuth.Load() && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") {
			u, p, ok := r.BasicAuth()
			if !ok {
				anonHits.Add(1)
				w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if u != "user" || p != "pass" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			authHits.Add(1)
		}
		registryHandler.ServeHTTP(w, r)
	}))
	defer server.Close()

	img := mutate.Annotations(testImage(t, "layer-auth"), map[string]string{"io.docker.agent.version": "test"}).(v1.Image)
	registryRef := strings.TrimPrefix(server.URL, "http://") + "/layer-auth:latest"
	require.NoError(t, crane.Push(img, registryRef, crane.Insecure))
	requireBlobAuth.Store(true)

	ref, err := name.ParseReference(registryRef, name.Insecure)
	require.NoError(t, err)

	keychain := staticKeychain{auth: &authn.Basic{Username: "user", Password: "pass"}}
	s := newTestSession(t, crane.WithAuthFromKeychain(keychain))
	img, err = s.image(t.Context(), ref)
	require.NoError(t, err)

	store, err := content.NewStore(content.WithBaseDir(t.TempDir()))
	require.NoError(t, err)

	dig, err := storeArtifact(t.Context(), store, s, ref, "layer-auth:latest", img)
	require.NoError(t, err)
	assert.NotEmpty(t, dig)
	assert.Positive(t, anonHits.Load(), "expected an anonymous layer attempt")
	assert.Positive(t, authHits.Load(), "expected a credentialed retry for the layer")
}

func TestSessionReusesAuthAcrossDigestAndImage(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	server := reg.start(t)

	registry := strings.TrimPrefix(server.URL, "http://")
	ref, err := name.ParseReference(registry + "/reuse:latest")
	require.NoError(t, err)

	s := newTestSession(t)

	dig, err := s.digest(t.Context(), ref)
	require.NoError(t, err)
	assert.Equal(t, reg.digest, dig)

	img, err := s.image(t.Context(), ref)
	require.NoError(t, err)
	_, err = img.Manifest()
	require.NoError(t, err)

	// digest (HEAD) and image (GET) should share one puller, so the registry
	// is pinged only once rather than once per crane call.
	assert.Equal(t, int64(1), reg.pingHits.Load(), "expected a single registry ping for the shared session")
}

// TestSessionFewerRequestsThanCraneCalls is a head-to-head proof of the
// optimization: it runs the digest + manifest steps once through the shared
// session and once through the previous approach (crane.Digest then crane.Pull)
// against identical registries, and asserts the session makes strictly fewer
// HTTP requests by avoiding the redundant ping/token round trips.
func TestSessionFewerRequestsThanCraneCalls(t *testing.T) {
	t.Parallel()

	// Shared-session approach.
	sessReg := newTestRegistry(t)
	sessServer := sessReg.start(t)
	sessRef, err := name.ParseReference(strings.TrimPrefix(sessServer.URL, "http://") + "/repo:latest")
	require.NoError(t, err)

	s := newTestSession(t)
	_, err = s.digest(t.Context(), sessRef)
	require.NoError(t, err)
	img, err := s.image(t.Context(), sessRef)
	require.NoError(t, err)
	_, err = img.Manifest()
	require.NoError(t, err)

	// Previous approach: two independent crane calls, each re-pinging and
	// re-authenticating from scratch.
	craneReg := newTestRegistry(t)
	craneServer := craneReg.start(t)
	craneRef := strings.TrimPrefix(craneServer.URL, "http://") + "/repo:latest"

	_, err = crane.Digest(craneRef, crane.Insecure)
	require.NoError(t, err)
	craneImg, err := crane.Pull(craneRef, crane.Insecure)
	require.NoError(t, err)
	_, err = craneImg.Manifest()
	require.NoError(t, err)

	t.Logf("requests: shared session=%d, two crane calls=%d", sessReg.reqHits.Load(), craneReg.reqHits.Load())
	assert.Less(t, sessReg.reqHits.Load(), craneReg.reqHits.Load(),
		"shared session should make fewer HTTP requests than two independent crane calls")
	assert.Equal(t, int64(1), sessReg.pingHits.Load(), "shared session should ping the registry once")
	assert.Greater(t, craneReg.pingHits.Load(), int64(1), "independent crane calls re-ping per call")
}

func TestSessionDigestResolvesPlatformIndex(t *testing.T) {
	t.Parallel()

	amd64Image := testImage(t, "amd64")
	arm64Image := testImage(t, "arm64")
	amd64Digest, err := amd64Image.Digest()
	require.NoError(t, err)
	arm64Digest, err := arm64Image.Digest()
	require.NoError(t, err)
	require.NotEqual(t, amd64Digest, arm64Digest)

	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{
			Add: amd64Image,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{
				OS:           "linux",
				Architecture: "amd64",
			}},
		},
		mutate.IndexAddendum{
			Add: arm64Image,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{
				OS:           "linux",
				Architecture: "arm64",
			}},
		},
	)

	server := httptest.NewServer(testregistry.New(testregistry.Logger(log.New(io.Discard, "", 0))))
	defer server.Close()

	registryRef := strings.TrimPrefix(server.URL, "http://") + "/platform:latest"
	ref, err := name.ParseReference(registryRef, name.Insecure)
	require.NoError(t, err)
	require.NoError(t, v1remote.WriteIndex(ref, idx))

	platform := &v1.Platform{OS: "linux", Architecture: "arm64"}
	s, err := newSession(crane.GetOptions(crane.Insecure, crane.WithPlatform(platform)))
	require.NoError(t, err)

	dig, err := s.digest(t.Context(), ref)
	require.NoError(t, err)
	assert.Equal(t, arm64Digest.String(), dig)
}

func testImage(t *testing.T, contents string) v1.Image {
	t.Helper()

	layer := static.NewLayer([]byte(contents), types.OCIUncompressedLayer)
	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	return img
}

func TestPullKeepsRegistryScopedReferences(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	serverA := httptest.NewServer(testregistry.New(testregistry.Logger(log.New(io.Discard, "", 0))))
	defer serverA.Close()
	serverB := httptest.NewServer(testregistry.New(testregistry.Logger(log.New(io.Discard, "", 0))))
	defer serverB.Close()

	refA := strings.TrimPrefix(serverA.URL, "http://") + "/same/agent:latest"
	refB := strings.TrimPrefix(serverB.URL, "http://") + "/same/agent:latest"
	imageA := mutate.Annotations(testImage(t, "registry-a"), map[string]string{"io.docker.agent.version": "test"}).(v1.Image)
	imageB := mutate.Annotations(testImage(t, "registry-b"), map[string]string{"io.docker.agent.version": "test"}).(v1.Image)
	require.NoError(t, crane.Push(imageA, refA, crane.Insecure))
	require.NoError(t, crane.Push(imageB, refB, crane.Insecure))

	_, err := Pull(t.Context(), refA, false, crane.Insecure)
	require.NoError(t, err)
	_, err = Pull(t.Context(), refB, false, crane.Insecure)
	require.NoError(t, err)

	store, err := content.NewStore()
	require.NoError(t, err)
	storedA, err := store.GetArtifact(refA)
	require.NoError(t, err)
	storedB, err := store.GetArtifact(refB)
	require.NoError(t, err)
	assert.Equal(t, "registry-a", storedA)
	assert.Equal(t, "registry-b", storedB)
	_, err = store.GetArtifact("same/agent:latest")
	require.ErrorIs(t, err, content.ErrStoreCorrupted, "Pull must not populate a registry-less legacy key")
}

func TestPullRegistryNotFound(t *testing.T) {
	t.Parallel()

	// Use a test server that returns 404 for fast failure
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	// Extract host:port from server URL (remove http://)
	registry := strings.TrimPrefix(server.URL, "http://")

	// Test various image references that should fail with 404
	refs := []string{
		registry + "/non-existent:latest",
		registry + "/test:latest",
	}

	for _, ref := range refs {
		_, err := Pull(t.Context(), ref, false, crane.Insecure)
		require.Error(t, err, "expected error for ref: %s", ref)
	}
}

func TestPullIntegration(t *testing.T) {
	t.Parallel()

	store, err := content.NewStore(content.WithBaseDir(t.TempDir()))
	require.NoError(t, err)

	testData := []byte("test pull integration")
	layer := static.NewLayer(testData, types.OCIUncompressedLayer)
	img := empty.Image
	img, err = mutate.AppendLayers(img, layer)
	require.NoError(t, err)

	testRef := "pull-test:latest"
	digest, err := store.StoreArtifact(img, testRef)
	require.NoError(t, err)

	t.Cleanup(func() {
		if err := store.DeleteArtifact(digest); err != nil {
			t.Logf("Failed to clean up artifact: %v", err)
		}
	})

	retrievedImg, err := store.GetArtifactImage(testRef)
	require.NoError(t, err)
	assert.NotNil(t, retrievedImg)

	err = Push(t.Context(), "invalid:reference:with:too:many:colons")
	require.Error(t, err)
}

func TestFullyQualifiedReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		ref      string
		expected string
	}{
		{
			name:     "short reference gets normalized",
			ref:      "myorg/review-pr",
			expected: "index.docker.io/myorg/review-pr:latest",
		},
		{
			name:     "fully qualified reference gets normalized to same key",
			ref:      "index.docker.io/myorg/review-pr:latest",
			expected: "index.docker.io/myorg/review-pr:latest",
		},
		{
			name:     "tagged reference preserves tag",
			ref:      "myorg/review-pr:v1",
			expected: "index.docker.io/myorg/review-pr:v1",
		},
		{
			name:     "digest reference preserves digest",
			ref:      "myorg/review-pr@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			expected: "index.docker.io/myorg/review-pr@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
		{
			name:     "non-docker-hub registry",
			ref:      "ghcr.io/myorg/agent:v2",
			expected: "ghcr.io/myorg/agent:v2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, err := FullyQualifiedReference(tt.ref)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFullyQualifiedReference_InvalidReference(t *testing.T) {
	t.Parallel()

	_, err := FullyQualifiedReference(":::invalid")
	require.Error(t, err)
}

func TestIsDigestReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		ref      string
		expected bool
	}{
		{"tag reference", "myorg/review-pr:latest", false},
		{"implicit tag", "myorg/review-pr", false},
		{"digest reference", "myorg/review-pr@sha256:0000000000000000000000000000000000000000000000000000000000000000", true},
		{"fully qualified digest", "index.docker.io/myorg/review-pr@sha256:0000000000000000000000000000000000000000000000000000000000000000", true},
		{"invalid reference", ":::invalid", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, IsDigestReference(tt.ref))
		})
	}
}
