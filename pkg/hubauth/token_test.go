package hubauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToken(t *testing.T) {
	t.Run("exchanges the stored access token", func(t *testing.T) {
		hub := installFakeHub(t, longLived(t))
		installSecret(t, testToken)

		token, err := Token(t.Context())
		require.NoError(t, err)
		assert.NotEmpty(t, token)
		assert.Equal(t, []credentials{{"bob", testToken}}, hub.received())
	})

	t.Run("reuses the minted token until it is about to expire", func(t *testing.T) {
		hub := installFakeHub(t, longLived(t))
		installSecret(t, testToken)

		first, err := Token(t.Context())
		require.NoError(t, err)
		second, err := Token(t.Context())
		require.NoError(t, err)

		assert.Equal(t, first, second)
		assert.Len(t, hub.received(), 1)
	})

	t.Run("mints again when the token is due for renewal", func(t *testing.T) {
		hub := installFakeHub(t, longLived(t))
		installSecret(t, testToken)

		_, err := Token(t.Context())
		require.NoError(t, err)
		expireRenewal()

		_, err = Token(t.Context())
		require.NoError(t, err)
		assert.Len(t, hub.received(), 2)
	})

	t.Run("credentials are re-checked but not re-exchanged", func(t *testing.T) {
		hub := installFakeHub(t, longLived(t))
		installSecret(t, testToken)

		first, err := Token(t.Context())
		require.NoError(t, err)
		expireCredentialCheck()

		second, err := Token(t.Context())
		require.NoError(t, err)
		assert.Equal(t, first, second)
		assert.Len(t, hub.received(), 1, "same credentials and token still fresh")
	})

	t.Run("a credential change mints a new token", func(t *testing.T) {
		hub := installFakeHub(t, longLived(t))
		installSecret(t, testToken)

		_, err := Token(t.Context())
		require.NoError(t, err)
		expireCredentialCheck()
		installSecret(t, tokenPrefix+"pat_other")

		_, err = Token(t.Context())
		require.NoError(t, err)
		assert.Len(t, hub.received(), 2)
	})

	t.Run("signing out drops the minted token", func(t *testing.T) {
		installFakeHub(t, longLived(t))
		installSecret(t, testToken)

		_, err := Token(t.Context())
		require.NoError(t, err)
		expireCredentialCheck()
		installSecret(t, "")

		_, err = Token(t.Context())
		require.ErrorIs(t, err, errNoCredentials)
	})

	t.Run("passwords are never exchanged", func(t *testing.T) {
		hub := installFakeHub(t, longLived(t))
		installSecret(t, "hunter2")

		_, err := Token(t.Context())
		require.ErrorIs(t, err, errNoCredentials)
		assert.Empty(t, hub.received())
	})

	t.Run("credential store failure is reported", func(t *testing.T) {
		installFakeHub(t, longLived(t))
		lookupCredentials = func() (string, string, error) { return "", "", errors.New("boom") }

		_, err := Token(t.Context())
		require.ErrorIs(t, err, errNoCredentials)
		assert.ErrorContains(t, err, "boom")
	})

	t.Run("a usable token survives a failed renewal", func(t *testing.T) {
		hub := installFakeHub(t, longLived(t))
		installSecret(t, testToken)

		first, err := Token(t.Context())
		require.NoError(t, err)

		hub.serve("")
		expireRenewal()
		second, err := Token(t.Context())
		require.NoError(t, err)
		assert.Equal(t, first, second)

		// Still served while the failure cooldown holds off new attempts.
		third, err := Token(t.Context())
		require.NoError(t, err)
		assert.Equal(t, first, third)
		assert.Len(t, hub.received(), 2)
	})

	t.Run("failures are cached to keep callers fast", func(t *testing.T) {
		hub := installFakeHub(t, "")
		installSecret(t, testToken)

		_, err := Token(t.Context())
		require.ErrorContains(t, err, "no token")

		_, err = Token(t.Context())
		require.ErrorContains(t, err, "before retrying")
		assert.Len(t, hub.received(), 1, "no new exchange while cooling down")
	})

	t.Run("a refused access token backs off for longer", func(t *testing.T) {
		hub := installFakeHub(t, longLived(t))
		installSecret(t, testToken)
		hub.fail(http.StatusUnauthorized, nil)

		_, err := Token(t.Context())
		require.ErrorIs(t, err, errRejected)
		require.ErrorContains(t, err, "docker login")

		state.Lock()
		cooldown := time.Until(state.nextAttempt)
		state.Unlock()
		assert.Greater(t, cooldown, failureCooldown, "a revoked token won't fix itself")
	})

	t.Run("concurrent callers share a single exchange", func(t *testing.T) {
		hub := installFakeHub(t, longLived(t))
		installSecret(t, testToken)

		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				_, err := Token(t.Context())
				assert.NoError(t, err)
			})
		}
		wg.Wait()

		assert.Len(t, hub.received(), 1)
	})

	t.Run("a canceled caller neither blocks nor poisons the others", func(t *testing.T) {
		release := make(chan struct{})
		resetState(t)
		loginEndpoint = newServer(t, func(w http.ResponseWriter, _ *http.Request) {
			<-release
			_ = json.NewEncoder(w).Encode(map[string]string{"token": longLived(t)})
		})
		installSecret(t, testToken)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := Token(ctx)
		require.ErrorIs(t, err, context.Canceled)

		close(release)
		token, err := Token(t.Context())
		require.NoError(t, err)
		assert.NotEmpty(t, token)
	})

	t.Run("the exchange can be turned off", func(t *testing.T) {
		hub := installFakeHub(t, longLived(t))
		installSecret(t, testToken)
		t.Setenv(envNoExchange, "1")

		_, err := Token(t.Context())
		require.ErrorContains(t, err, envNoExchange)
		assert.Empty(t, hub.received())
	})
}

func TestInvalidate(t *testing.T) {
	t.Run("drops the token the caller found unusable", func(t *testing.T) {
		hub := installFakeHub(t, longLived(t))
		installSecret(t, testToken)

		token, err := Token(t.Context())
		require.NoError(t, err)

		Invalidate(token)
		_, err = Token(t.Context())
		require.NoError(t, err)
		assert.Len(t, hub.received(), 2)
	})

	t.Run("keeps a token that was already replaced", func(t *testing.T) {
		hub := installFakeHub(t, longLived(t))
		installSecret(t, testToken)

		token, err := Token(t.Context())
		require.NoError(t, err)

		Invalidate("some-other-token")
		fresh, err := Token(t.Context())
		require.NoError(t, err)
		assert.Equal(t, token, fresh)
		assert.Len(t, hub.received(), 1)
	})
}
