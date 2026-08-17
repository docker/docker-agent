package hubauth

import (
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ExpiryLeeway is how long before its expiry a token stops being handed out:
// it covers the flight time of the request the token authenticates, plus the
// residual clock difference with the issuer.
const ExpiryLeeway = 30 * time.Second

// Expiry returns the token's exp claim, or false when the token doesn't parse
// or carries no exp claim.
func Expiry(token string) (time.Time, bool) {
	claims, err := parseClaims(token)
	if err != nil {
		return time.Time{}, false
	}
	exp, err := claims.GetExpirationTime()
	if err != nil || exp == nil {
		return time.Time{}, false
	}
	return exp.Time, true
}

// Expiring reports whether the JWT's exp claim has passed or is less than
// [ExpiryLeeway] away, i.e. whether a fresh token should be obtained. Tokens
// that don't parse or carry no exp claim are left for the server to judge.
func Expiring(token string) bool {
	exp, ok := Expiry(token)
	if !ok {
		return false
	}
	return exp.Before(now().Add(ExpiryLeeway))
}

// renewAt returns the time from which token must be replaced.
func renewAt(token string) time.Time {
	exp, ok := Expiry(token)
	if !ok {
		return now().Add(unknownExpiryTTL)
	}
	return exp.Add(-renewBefore)
}

// parseClaims reads a JWT's claims without verifying its signature: the token
// is a bearer credential we received over TLS from its issuer, and only the
// issuer can act on it.
func parseClaims(token string) (jwt.MapClaims, error) {
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, claims); err != nil {
		return nil, err
	}
	return claims, nil
}
