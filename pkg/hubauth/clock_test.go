package hubauth

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLearnClockSkew(t *testing.T) {
	t.Run("ignores small differences", func(t *testing.T) {
		clockSkew.Store(0)
		t.Cleanup(func() { clockSkew.Store(0) })

		learnClockSkew(dateHeader(time.Now().Add(time.Second)))
		assert.Zero(t, clockSkew.Load())
		assert.WithinDuration(t, time.Now(), now(), time.Second)
	})

	t.Run("corrects a clock that is behind", func(t *testing.T) {
		clockSkew.Store(0)
		t.Cleanup(func() { clockSkew.Store(0) })

		learnClockSkew(dateHeader(time.Now().Add(time.Hour)))
		assert.WithinDuration(t, time.Now().Add(time.Hour), now(), 5*time.Second)
	})

	t.Run("corrects a clock that is ahead", func(t *testing.T) {
		clockSkew.Store(0)
		t.Cleanup(func() { clockSkew.Store(0) })

		learnClockSkew(dateHeader(time.Now().Add(-time.Hour)))
		assert.WithinDuration(t, time.Now().Add(-time.Hour), now(), 5*time.Second)
	})

	t.Run("ignores a missing or unparseable header", func(t *testing.T) {
		clockSkew.Store(int64(time.Minute))
		t.Cleanup(func() { clockSkew.Store(0) })

		learnClockSkew(http.Header{})
		learnClockSkew(http.Header{"Date": []string{"nonsense"}})
		assert.Equal(t, int64(time.Minute), clockSkew.Load(), "an unusable header leaves the known skew alone")
	})
}

// TestExpiryDecisionsFollowTheIssuersClock covers the reason clock skew is
// tracked: a badly skewed machine would otherwise consider every fresh token
// expired.
func TestExpiryDecisionsFollowTheIssuersClock(t *testing.T) {
	clockSkew.Store(0)
	t.Cleanup(func() { clockSkew.Store(0) })

	// Our clock runs an hour ahead of Docker's, so a token just issued with ten
	// minutes of life looks long dead.
	token := makeToken(t, time.Now().Add(-time.Hour+10*time.Minute))
	assert.True(t, Expiring(token))

	learnClockSkew(dateHeader(time.Now().Add(-time.Hour)))
	assert.False(t, Expiring(token), "once the skew is known, the token is fine")
}

func dateHeader(at time.Time) http.Header {
	return http.Header{"Date": []string{at.UTC().Format(http.TimeFormat)}}
}

// TestClockSkewIsLearnedFromTokenResponsesOnly pins where the correction may
// come from: only a 200 from the exchange endpoint proves we reached Docker, so
// an error page — from a TLS-terminating proxy, say — must not move this
// process's idea of the time, which every expiry decision depends on.
func TestClockSkewIsLearnedFromTokenResponsesOnly(t *testing.T) {
	t.Run("learns from a token response", func(t *testing.T) {
		// Long-lived enough to stay valid once our clock is corrected forward.
		hub := installFakeHub(t, makeToken(t, time.Now().Add(3*time.Hour)))
		installSecret(t, testToken)
		hub.respondWith(dateHeader(time.Now().Add(time.Hour)))

		_, err := Token(t.Context())
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now().Add(time.Hour), now(), 5*time.Second)
	})

	t.Run("ignores an error response", func(t *testing.T) {
		hub := installFakeHub(t, longLived(t))
		installSecret(t, testToken)
		hub.fail(http.StatusProxyAuthRequired, dateHeader(time.Now().Add(time.Hour)))

		_, err := Token(t.Context())
		require.Error(t, err)
		assert.Zero(t, clockSkew.Load())
	})
}
