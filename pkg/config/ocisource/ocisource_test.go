package ocisource

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/configsize"
	"github.com/docker/docker-agent/pkg/content"
	"github.com/docker/docker-agent/pkg/memoize"
	"github.com/docker/docker-agent/pkg/protect"
	"github.com/docker/docker-agent/pkg/remote"
)

func TestOCISource_Read_OversizedArtifactIsNotRedownloaded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	resetOCIMemoizer(t)

	const ref = "test-oversized/agent:latest"
	storeTestArtifact(t, ref, make([]byte, configsize.MaxBytes+1))
	var pulls atomic.Int32
	stubOCIPull(t, func(context.Context, string, bool) (string, error) {
		pulls.Add(1)
		return "", nil
	})

	_, err := New(ref).Read(t.Context())
	require.ErrorIs(t, err, configsize.ErrTooLarge)
	require.NotErrorIs(t, err, content.ErrStoreCorrupted)
	assert.Equal(t, int32(1), pulls.Load(), "an oversized valid artifact must not trigger a forced re-pull")
}

func TestOCISource_DigestReference_ServesFromCache(t *testing.T) {
	t.Parallel()

	// Create a temporary content store and store a test artifact.
	storeDir := t.TempDir()
	store, err := content.NewStore(content.WithBaseDir(storeDir))
	require.NoError(t, err)

	testData := []byte("version: v1\nname: test-agent")
	layer := static.NewLayer(testData, "application/yaml")
	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	img = mutate.Annotations(img, map[string]string{
		"io.docker.agent.version": "test",
	}).(v1.Image)

	ref := "registry.example.com/test-digest-cache/agent:latest"
	storeKey, err := remote.FullyQualifiedReference(ref)
	require.NoError(t, err)
	digest, err := store.StoreArtifact(img, storeKey)
	require.NoError(t, err)

	// Build a digest reference using the stored digest.
	digestRef := "registry.example.com/test-digest-cache/agent@" + digest

	// Read via ociSource. Since the reference is pinned by digest and is
	// present in the local store, this must succeed without any network call.
	// We override the default store directory via an env-based approach;
	// instead, we directly exercise the cache-hit logic by verifying the
	// store lookup works with the normalized key.
	storeKey, err = remote.FullyQualifiedReference(digestRef)
	require.NoError(t, err)

	// Verify the store can resolve the digest key directly.
	data, err := store.GetArtifact(storeKey)
	require.NoError(t, err)
	assert.Equal(t, string(testData), data)

	// Also verify that IsDigestReference correctly identifies this.
	assert.True(t, remote.IsDigestReference(digestRef))
	assert.False(t, remote.IsDigestReference(ref))
}

// storeTestArtifact writes an agent YAML artifact into the default content
// store (rooted at $HOME, which the caller must point at a temp dir) under
// the normalized registry-scoped key for ref.
func storeTestArtifact(t *testing.T, ref string, data []byte) {
	t.Helper()

	storeKey, err := remote.FullyQualifiedReference(ref)
	require.NoError(t, err)
	storeTestArtifactWithKey(t, storeKey, data)
}

func storeTestArtifactWithKey(t *testing.T, storeKey string, data []byte) {
	t.Helper()

	store, err := content.NewStore()
	require.NoError(t, err)

	layer := static.NewLayer(data, "application/yaml")
	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	img = mutate.Annotations(img, map[string]string{
		"io.docker.agent.version": "test",
	}).(v1.Image)

	_, err = store.StoreArtifact(img, storeKey)
	require.NoError(t, err)
}

// stubOCIPull replaces the registry pull with fn for the test's duration.
func stubOCIPull(t *testing.T, fn func(ctx context.Context, ref string, force bool) (string, error)) {
	t.Helper()
	original := pullOCIArtifact
	pullOCIArtifact = fn
	t.Cleanup(func() { pullOCIArtifact = original })
}

// resetOCIMemoizer swaps in a fresh read memoizer so tests neither observe
// nor leak cached reads across tests and repeated runs (-count=2).
func resetOCIMemoizer(t *testing.T) {
	t.Helper()
	original := ociReadMemoizer
	ociReadMemoizer = memoize.New[[]byte](time.Minute)
	t.Cleanup(func() { ociReadMemoizer = original })
}

// Not parallel: stubs the package-level pullOCIArtifact and re-homes the
// default content store via t.Setenv.
func TestOCISource_Read_MemoizesSuccessfulReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	resetOCIMemoizer(t)

	testData := []byte("version: v1\nname: memoized-agent")
	ref := "test-memoize/agent:latest"
	storeTestArtifact(t, ref, testData)

	var pulls atomic.Int32
	stubOCIPull(t, func(context.Context, string, bool) (string, error) {
		pulls.Add(1)
		return "", nil
	})

	source := New(ref)
	for range 3 {
		data, err := source.Read(t.Context())
		require.NoError(t, err)
		assert.Equal(t, testData, data)
	}
	assert.Equal(t, int32(1), pulls.Load(), "repeated reads of the same ref must pull only once")

	// An equivalent form of the same reference must hit the same cache entry.
	data, err := New("index.docker.io/test-memoize/agent:latest").Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, testData, data)
	assert.Equal(t, int32(1), pulls.Load(), "equivalent refs must share the cache entry")
}

// Not parallel: stubs the package-level pullOCIArtifact and re-homes the
// default content store via t.Setenv.
func TestOCISource_Read_DoesNotCacheFailures(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	resetOCIMemoizer(t)

	ref := "test-memoize-errors/agent:latest"

	// No local artifact and a failing pull: Read must fail.
	var pulls atomic.Int32
	stubOCIPull(t, func(context.Context, string, bool) (string, error) {
		pulls.Add(1)
		return "", errors.New("registry unreachable")
	})

	source := New(ref)
	_, err := source.Read(t.Context())
	require.Error(t, err)

	// Make the artifact available and the pull succeed: the earlier failure
	// must not have been cached, so Read retries and succeeds.
	testData := []byte("version: v1\nname: retried-agent")
	storeTestArtifact(t, ref, testData)
	stubOCIPull(t, func(context.Context, string, bool) (string, error) {
		pulls.Add(1)
		return "", nil
	})

	data, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, testData, data)
	assert.Equal(t, int32(2), pulls.Load(), "a failed read must be retried, not served from cache")
}

// Not parallel: stubs the package-level pullOCIArtifact and re-homes the
// default content store via t.Setenv.
func TestOCISource_Read_CacheIsScopedToRegistry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	resetOCIMemoizer(t)

	// Same repository and tag on two different registries: distinct trust
	// boundaries, so each must do its own pull instead of sharing an entry.
	testData := []byte("version: v1\nname: scoped-agent")
	storeTestArtifact(t, "registry-a.example.com/test-scoped/agent:latest", testData)
	storeTestArtifact(t, "registry-b.example.com/test-scoped/agent:latest", testData)

	var pulls atomic.Int32
	stubOCIPull(t, func(context.Context, string, bool) (string, error) {
		pulls.Add(1)
		return "", nil
	})

	_, err := New("registry-a.example.com/test-scoped/agent:latest").Read(t.Context())
	require.NoError(t, err)
	_, err = New("registry-b.example.com/test-scoped/agent:latest").Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int32(2), pulls.Load(), "refs differing only by registry must not share a cache entry")
}

// Not parallel: stubs the package-level pullOCIArtifact and re-homes the
// default content store via t.Setenv.
func TestOCISource_Read_FallbackIsScopedToRegistry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	resetOCIMemoizer(t)

	refA := "registry-a.example.com/test-fallback/agent:latest"
	refB := "registry-b.example.com/test-fallback/agent:latest"
	storeTestArtifact(t, refA, []byte("version: v1\nname: registry-a"))
	storeTestArtifact(t, refB, []byte("version: v1\nname: registry-b"))
	stubOCIPull(t, func(context.Context, string, bool) (string, error) {
		return "", errors.New("registry unreachable")
	})

	data, err := New(refA).Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []byte("version: v1\nname: registry-a"), data)
	data, err = New(refB).Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []byte("version: v1\nname: registry-b"), data)

	store, err := content.NewStore()
	require.NoError(t, err)
	_, err = store.GetArtifact("test-fallback/agent:latest")
	require.ErrorIs(t, err, content.ErrStoreCorrupted, "registry-less legacy keys must not be populated or used as fallback")
}

// Not parallel: stubs the package-level pullOCIArtifact and re-homes the
// default content store via t.Setenv.
func TestOCISource_Read_DoesNotCacheDegradedFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	resetOCIMemoizer(t)

	// Artifact is available locally but the registry is unreachable: Read
	// succeeds by falling back to the local store.
	testData := []byte("version: v1\nname: degraded-agent")
	ref := "test-degraded/agent:latest"
	storeTestArtifact(t, ref, testData)

	var pulls atomic.Int32
	stubOCIPull(t, func(context.Context, string, bool) (string, error) {
		pulls.Add(1)
		return "", errors.New("registry unreachable")
	})

	source := New(ref)
	for i := 1; i <= 2; i++ {
		data, err := source.Read(t.Context())
		require.NoError(t, err)
		assert.Equal(t, testData, data)
		assert.Equal(t, int32(i), pulls.Load(), "a degraded fallback read must not be cached; the registry must be retried")
	}

	// Once the registry recovers, the validated read IS cached.
	stubOCIPull(t, func(context.Context, string, bool) (string, error) {
		pulls.Add(1)
		return "", nil
	})
	for range 2 {
		data, err := source.Read(t.Context())
		require.NoError(t, err)
		assert.Equal(t, testData, data)
	}
	assert.Equal(t, int32(3), pulls.Load(), "a validated read must be cached again")
}

// storeProtectedTestArtifact is like storeTestArtifact but adds protection
// annotations produced by key in the given mode.
func storeProtectedTestArtifact(t *testing.T, ref string, data []byte, key *protect.Key, mode protect.Mode) {
	t.Helper()

	annotations := map[string]string{}
	require.NoError(t, key.Protect(annotations, data, mode))
	storeTestArtifactWithAnnotations(t, ref, data, annotations)
}

// storeTestArtifactWithAnnotations is like storeTestArtifact with extra
// manifest annotations.
func storeTestArtifactWithAnnotations(t *testing.T, ref string, data []byte, annotations map[string]string) {
	t.Helper()

	annotations["io.docker.agent.version"] = "test"
	storeKey, err := remote.FullyQualifiedReference(ref)
	require.NoError(t, err)

	store, err := content.NewStore()
	require.NoError(t, err)
	layer := static.NewLayer(data, "application/yaml")
	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	img = mutate.Annotations(img, annotations).(v1.Image)

	_, err = store.StoreArtifact(img, storeKey)
	require.NoError(t, err)
}

// Not parallel: stubs the package-level pullOCIArtifact and re-homes the
// default content store via t.Setenv.
func TestOCISource_Read_VerifiesProtection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	resetOCIMemoizer(t)
	stubOCIPull(t, func(context.Context, string, bool) (string, error) { return "", nil })

	key, err := protect.ParseKey([]byte("a shared secret long enough"))
	require.NoError(t, err)
	wrongKey, err := protect.ParseKey([]byte("a wrong secret long enough"))
	require.NoError(t, err)

	testData := []byte("version: v1\nname: signed-agent")
	signedRef := "test-signed/agent:latest"
	storeProtectedTestArtifact(t, signedRef, testData, key, protect.ModeSign)
	encryptedRef := "test-encrypted/agent:latest"
	storeProtectedTestArtifact(t, encryptedRef, testData, key, protect.ModeEncrypt)
	unsignedRef := "test-unsigned/agent:latest"
	storeTestArtifact(t, unsignedRef, testData)

	// Matching key: verified read succeeds.
	data, err := New(signedRef, WithVerificationKey(key)).Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, testData, data)

	// Wrong key: rejected, even though a verified read was just cached under
	// the other key.
	_, err = New(signedRef, WithVerificationKey(wrongKey)).Read(t.Context())
	require.ErrorIs(t, err, protect.ErrInvalidSignature)

	// No key: signature is ignored.
	data, err = New(signedRef).Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, testData, data)

	// Encrypted mode: verified by decrypting and comparing to the layer.
	data, err = New(encryptedRef, WithVerificationKey(key)).Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, testData, data)
	_, err = New(encryptedRef, WithVerificationKey(wrongKey)).Read(t.Context())
	require.ErrorIs(t, err, protect.ErrDecryption)

	// Key given but artifact unprotected: rejected.
	_, err = New(unsignedRef, WithVerificationKey(key)).Read(t.Context())
	require.ErrorIs(t, err, protect.ErrNotProtected)
}

// Not parallel: stubs the package-level pullOCIArtifact and re-homes the
// default content store via t.Setenv.
func TestOCISource_Read_CacheDistinguishesPrivateAndPublicKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	resetOCIMemoizer(t)
	stubOCIPull(t, func(context.Context, string, bool) (string, error) { return "", nil })

	ecPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	privDER, err := x509.MarshalPKCS8PrivateKey(ecPriv)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(&ecPriv.PublicKey)
	require.NoError(t, err)
	priv, err := protect.ParseKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}))
	require.NoError(t, err)
	pub, err := protect.ParseKey(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	require.NoError(t, err)

	// A correctly signed artifact whose encrypted copy was swapped for an
	// encryption of different content. The public key can only check the
	// signature and accepts it; the private key decrypts and must reject it.
	testData := []byte("version: v1\nname: swapped-copy")
	annotations := map[string]string{}
	require.NoError(t, priv.Protect(annotations, testData, protect.ModeEncrypt))
	forged, err := pub.Encrypt([]byte("something else"))
	require.NoError(t, err)
	annotations[protect.AnnotationEncrypted] = base64.StdEncoding.EncodeToString(forged)
	ref := "test-halves/agent:latest"
	storeTestArtifactWithAnnotations(t, ref, testData, annotations)

	data, err := New(ref, WithVerificationKey(pub)).Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, testData, data)

	// The public-key success above must not be served to the private key.
	_, err = New(ref, WithVerificationKey(priv)).Read(t.Context())
	require.ErrorIs(t, err, protect.ErrTampered)
}
