package hubauth

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
)

func TestSharedCache(t *testing.T) {
	t.Run("round-trips a token", func(t *testing.T) {
		resetState(t)
		token := longLived(t)

		store("fingerprint", token)
		got, ok := load("fingerprint")
		require.True(t, ok)
		assert.Equal(t, token, got)
	})

	t.Run("ignores another account's token", func(t *testing.T) {
		resetState(t)

		store("fingerprint", longLived(t))
		_, ok := load("other-fingerprint")
		assert.False(t, ok)
	})

	t.Run("ignores a token due for renewal", func(t *testing.T) {
		resetState(t)

		store("fingerprint", makeToken(t, time.Now().Add(renewBefore/2)))
		_, ok := load("fingerprint")
		assert.False(t, ok)
	})

	t.Run("ignores a token that isn't a Docker Hub token", func(t *testing.T) {
		resetState(t)

		store("fingerprint", makeToken(t, time.Now().Add(time.Hour), func(c jwt.MapClaims) {
			c["iss"] = "https://evil.example.com/"
		}))
		_, ok := load("fingerprint")
		assert.False(t, ok, "the file gets the same checks as a fresh exchange")
	})

	t.Run("survives a missing or corrupt file", func(t *testing.T) {
		resetState(t)

		_, ok := load("fingerprint")
		assert.False(t, ok)

		require.NoError(t, os.MkdirAll(filepath.Dir(cachePath()), 0o700))
		require.NoError(t, os.WriteFile(cachePath(), []byte("{not json"), 0o600))
		_, ok = load("fingerprint")
		assert.False(t, ok)
	})

	t.Run("is owner-only", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("file modes are POSIX-only")
		}
		resetState(t)
		// A cache directory as the rest of docker-agent leaves it: the token
		// must not rely on the root being owner-only.
		require.NoError(t, os.Chmod(paths.GetCacheDir(), 0o755))

		store("fingerprint", longLived(t))

		info, err := os.Stat(cachePath())
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

		dir, err := os.Stat(filepath.Dir(cachePath()))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), dir.Mode().Perm())
	})

	t.Run("forget removes the file", func(t *testing.T) {
		resetState(t)
		store("fingerprint", longLived(t))

		forget()
		_, err := os.Stat(cachePath())
		assert.True(t, os.IsNotExist(err))

		forget() // idempotent
	})
}

func TestTokenReusesAnotherProcessesToken(t *testing.T) {
	hub := installFakeHub(t, longLived(t))
	installSecret(t, testToken)

	shared := longLived(t)
	store(fingerprint("bob", testToken), shared)

	token, err := Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, shared, token)
	assert.Empty(t, hub.received(), "no exchange needed")
}

func TestTokenPublishesForOtherProcesses(t *testing.T) {
	installFakeHub(t, longLived(t))
	installSecret(t, testToken)

	token, err := Token(t.Context())
	require.NoError(t, err)

	cached, ok := load(fingerprint("bob", testToken))
	require.True(t, ok)
	assert.Equal(t, token, cached)
}

func TestInvalidateRemovesTheSharedToken(t *testing.T) {
	installFakeHub(t, longLived(t))
	installSecret(t, testToken)

	token, err := Token(t.Context())
	require.NoError(t, err)
	Invalidate(token)

	_, ok := load(fingerprint("bob", testToken))
	assert.False(t, ok, "other processes must not keep using a rejected token")
}
