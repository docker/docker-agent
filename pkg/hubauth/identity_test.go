package hubauth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
)

func TestIdentityFromToken(t *testing.T) {
	t.Run("reads the account from the claims", func(t *testing.T) {
		identity, ok := IdentityFromToken(longLived(t))

		assert.True(t, ok)
		assert.Equal(t, Identity{Username: "bob", Email: "bob@example.com"}, identity)
	})

	t.Run("accepts a token without an email", func(t *testing.T) {
		token := makeToken(t, time.Now().Add(time.Hour), func(c jwt.MapClaims) {
			c[hubClaim] = map[string]any{"username": "bob"}
		})

		identity, ok := IdentityFromToken(token)
		assert.True(t, ok)
		assert.Equal(t, Identity{Username: "bob"}, identity)
	})

	t.Run("reports tokens without account information", func(t *testing.T) {
		for name, token := range map[string]string{
			"not a JWT":     "not-a-jwt",
			"no hub claim":  makeToken(t, time.Now().Add(time.Hour), func(c jwt.MapClaims) { delete(c, hubClaim) }),
			"empty claim":   makeToken(t, time.Now().Add(time.Hour), func(c jwt.MapClaims) { c[hubClaim] = map[string]any{} }),
			"claim is text": makeToken(t, time.Now().Add(time.Hour), func(c jwt.MapClaims) { c[hubClaim] = "nope" }),
		} {
			t.Run(name, func(t *testing.T) {
				_, ok := IdentityFromToken(token)
				assert.False(t, ok)
			})
		}
	})
}

func TestIsAccessToken(t *testing.T) {
	assert.True(t, isAccessToken("dckr_pat_abc"))
	assert.True(t, isAccessToken("dckr_oat_abc"), "org access tokens are tokens too")
	assert.False(t, isAccessToken("hunter2"))
	assert.False(t, isAccessToken(""))
}

func TestFingerprintSeparatesFields(t *testing.T) {
	// Without a separator, ("ab", "c") and ("a", "bc") would collide.
	assert.NotEqual(t, fingerprint("ab", "c"), fingerprint("a", "bc"))
}
