package hubauth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
)

func TestExpiringAt(t *testing.T) {
	previousSkew := clockSkew.Swap(0)
	t.Cleanup(func() { clockSkew.Store(previousSkew) })

	for _, tc := range []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{name: "zero", want: true},
		{name: "expired", expiresAt: time.Now().Add(-time.Minute), want: true},
		{name: "near expiry", expiresAt: time.Now().Add(ExpiryLeeway / 2), want: true},
		{name: "beyond leeway", expiresAt: time.Now().Add(2 * ExpiryLeeway)},
		{name: "future", expiresAt: time.Now().Add(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ExpiringAt(tc.expiresAt))
			assert.Equal(t, tc.want, Expiring(makeToken(t, tc.expiresAt)))
		})
	}
}

func TestExpiringAtClockSkew(t *testing.T) {
	previousSkew := clockSkew.Load()
	t.Cleanup(func() { clockSkew.Store(previousSkew) })

	for _, tc := range []struct {
		name          string
		expiresIn     time.Duration
		initialSkew   time.Duration
		correctedSkew time.Duration
		wantInitial   bool
		wantCorrected bool
	}{
		{
			name:          "local clock ahead",
			expiresIn:     -50 * time.Minute,
			correctedSkew: -time.Hour,
			wantInitial:   true,
		},
		{
			name:          "local clock behind",
			expiresIn:     10 * time.Minute,
			correctedSkew: time.Hour,
			wantCorrected: true,
		},
		{
			name:          "previously accepted expiry after correction changes",
			expiresIn:     -50 * time.Minute,
			initialSkew:   -time.Hour,
			wantCorrected: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expiresAt := time.Now().Add(tc.expiresIn)
			token := makeToken(t, expiresAt)

			clockSkew.Store(int64(tc.initialSkew))
			assert.Equal(t, tc.wantInitial, ExpiringAt(expiresAt))
			assert.Equal(t, tc.wantInitial, Expiring(token))

			clockSkew.Store(int64(tc.correctedSkew))
			assert.Equal(t, tc.wantCorrected, ExpiringAt(expiresAt))
			assert.Equal(t, tc.wantCorrected, Expiring(token))
		})
	}
}

func TestExpiringUnknownExpiry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "empty"},
		{name: "unparseable", token: "not-a-jwt"},
		{
			name: "missing exp",
			token: makeToken(t, time.Now(), func(claims jwt.MapClaims) {
				delete(claims, "exp")
			}),
		},
		{
			name: "invalid exp",
			token: makeToken(t, time.Now(), func(claims jwt.MapClaims) {
				claims["exp"] = "invalid"
			}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.False(t, Expiring(tc.token))
		})
	}
}
