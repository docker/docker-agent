package hubauth

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExchangeRejectsUnusableTokens(t *testing.T) {
	later := time.Now().Add(time.Hour)

	tests := []struct {
		name  string
		token string
		want  string
	}{
		{
			name: "no token",
			want: "no token",
		},
		{
			name:  "not a JWT",
			token: "not-a-jwt",
			want:  "unreadable",
		},
		{
			name:  "unexpected issuer",
			token: makeToken(t, later, func(c jwt.MapClaims) { c["iss"] = "https://evil.example.com/" }),
			want:  "unexpected issuer",
		},
		{
			name:  "unexpected audience",
			token: makeToken(t, later, func(c jwt.MapClaims) { c["aud"] = []string{"https://evil.example.com"} }),
			want:  "unexpected audience",
		},
		{
			name:  "too close to expiry",
			token: makeToken(t, time.Now().Add(ExpiryLeeway/2)),
			want:  "too close to expiry",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installFakeHub(t, tt.token)
			installSecret(t, testToken)

			_, err := Token(t.Context())
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

func TestExchangeRetriesTransientFailures(t *testing.T) {
	t.Run("retries a server error", func(t *testing.T) {
		token := longLived(t)
		var attempts int
		resetState(t)
		loginEndpoint = newServer(t, func(w http.ResponseWriter, _ *http.Request) {
			attempts++
			if attempts == 1 {
				http.Error(w, "boom", http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
		})
		installSecret(t, testToken)

		got, err := Token(t.Context())
		require.NoError(t, err)
		assert.Equal(t, token, got)
		assert.Equal(t, 2, attempts)
	})

	t.Run("gives up after maxAttempts", func(t *testing.T) {
		hub := installFakeHub(t, longLived(t))
		installSecret(t, testToken)
		hub.fail(http.StatusInternalServerError, nil)

		_, err := Token(t.Context())
		require.ErrorIs(t, err, errTransient)
		assert.Len(t, hub.received(), maxAttempts)
	})

	t.Run("does not retry a refusal", func(t *testing.T) {
		hub := installFakeHub(t, longLived(t))
		installSecret(t, testToken)
		hub.fail(http.StatusForbidden, nil)

		_, err := Token(t.Context())
		require.ErrorIs(t, err, errRejected)
		assert.Len(t, hub.received(), 1)
	})

	t.Run("honours a short Retry-After", func(t *testing.T) {
		hub := installFakeHub(t, longLived(t))
		installSecret(t, testToken)
		hub.fail(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"0"}})

		_, err := Token(t.Context())
		require.ErrorIs(t, err, errTransient)
		assert.Len(t, hub.received(), maxAttempts)
	})

	t.Run("gives up on a long Retry-After", func(t *testing.T) {
		hub := installFakeHub(t, longLived(t))
		installSecret(t, testToken)
		hub.fail(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"600"}})

		_, err := Token(t.Context())
		require.ErrorIs(t, err, errTransient)
		assert.Len(t, hub.received(), 1, "the server asked us to stay away")
	})
}

func TestRetryAfterFrom(t *testing.T) {
	assert.Zero(t, retryAfterFrom(http.Header{}))
	assert.Equal(t, 2*time.Second, retryAfterFrom(http.Header{"Retry-After": []string{"2"}}))
	assert.Zero(t, retryAfterFrom(http.Header{"Retry-After": []string{"-2"}}))
	assert.Zero(t, retryAfterFrom(http.Header{"Retry-After": []string{"nonsense"}}))

	date := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	assert.InDelta(t, 90*time.Second, retryAfterFrom(http.Header{"Retry-After": []string{date}}), float64(2*time.Second))

	// A date is only meaningful next to the clock it was written by: an hour of
	// difference between the server and this machine must not turn a 90-second
	// delay into a whole hour, nor into none at all.
	for _, offset := range []time.Duration{time.Hour, -time.Hour} {
		serverNow := time.Now().Add(offset)
		header := http.Header{
			"Date":        []string{serverNow.UTC().Format(http.TimeFormat)},
			"Retry-After": []string{serverNow.Add(90 * time.Second).UTC().Format(http.TimeFormat)},
		}
		assert.InDelta(t, 90*time.Second, retryAfterFrom(header), float64(2*time.Second))
	}
}

func TestExchangeDoesNotFollowRedirects(t *testing.T) {
	resetState(t)

	var leaked bool
	target := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		leaked = true
		_ = json.NewEncoder(w).Encode(map[string]string{"token": longLived(t)})
	})
	loginEndpoint = newServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target(), http.StatusTemporaryRedirect)
	})
	installSecret(t, testToken)

	_, err := Token(t.Context())
	require.Error(t, err)
	assert.False(t, leaked, "the access token must not reach the redirect target")
}

func TestLoginEndpointOverride(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "unset", url: "", want: defaultLoginEndpoint},
		{name: "docker host", url: "https://hub-stage.docker.com/v2/users/login", want: "https://hub-stage.docker.com/v2/users/login"},
		{name: "other host", url: "https://evil.example.com/login", want: defaultLoginEndpoint},
		{name: "plain HTTP", url: "http://hub.docker.com/v2/users/login", want: defaultLoginEndpoint},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envLoginURL, tt.url)
			assert.Equal(t, tt.want, resolveLoginEndpoint())
		})
	}
}
