package chatgpt

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/cli/invocation"
)

// makeJWT builds an unsigned JWT carrying the given claims. Signature
// verification is never performed on these tokens, so "sig" is enough.
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func chatgptClaims(accountID, plan string) map[string]any {
	return map[string]any{
		openAIAuthClaim: map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  plan,
		},
	}
}

// useTempCredentials points credential storage at a temp file and returns
// its path.
func useTempCredentials(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chatgpt-auth.json")
	restore := SetCredentialsPathForTests(path)
	t.Cleanup(restore)
	return path
}

func writeCredentials(t *testing.T, creds *Credentials) {
	t.Helper()
	require.NoError(t, save(creds))
}

func TestParseClaims(t *testing.T) {
	claims := chatgptClaims("acc_123", "plus")
	claims["email"] = "user@example.com"
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	token := makeJWT(t, claims)

	parsed := parseClaims(token)

	assert.Equal(t, "acc_123", parsed.AccountID)
	assert.Equal(t, "plus", parsed.Plan)
	assert.Equal(t, "user@example.com", parsed.Email)
	assert.WithinDuration(t, time.Now().Add(time.Hour), parsed.ExpiresAt, 5*time.Second)

	assert.Zero(t, parseClaims(""), "empty token yields zero claims")
	assert.Zero(t, parseClaims("not-a-jwt"), "malformed token yields zero claims")
}

func TestAccountIDFromToken(t *testing.T) {
	token := makeJWT(t, chatgptClaims("acc_42", "pro"))
	assert.Equal(t, "acc_42", AccountIDFromToken(token))
	assert.Empty(t, AccountIDFromToken(makeJWT(t, map[string]any{"email": "x@y.z"})))
}

func TestCredentialsStoreRoundTrip(t *testing.T) {
	path := useTempCredentials(t)

	_, err := Load()
	require.ErrorIs(t, err, ErrNotLoggedIn)
	assert.False(t, LoggedIn())

	creds := &Credentials{
		AccessToken:  "access",
		RefreshToken: "refresh",
		AccountID:    "acc_1",
		Email:        "user@example.com",
		Plan:         "plus",
		ExpiresAt:    time.Now().Add(time.Hour).UTC(),
	}
	writeCredentials(t, creds)

	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "credentials must be owner-only")
	}

	loaded, err := Load()
	require.NoError(t, err)
	assert.Equal(t, creds.AccessToken, loaded.AccessToken)
	assert.Equal(t, creds.Email, loaded.Email)
	assert.True(t, LoggedIn())

	removed, err := Logout()
	require.NoError(t, err)
	assert.True(t, removed)
	assert.False(t, LoggedIn())

	removed, err = Logout()
	require.NoError(t, err)
	assert.False(t, removed, "second logout finds nothing to remove")
}

func TestLoadCorruptFile(t *testing.T) {
	path := useTempCredentials(t)
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	_, err := Load()
	require.ErrorContains(t, err, "corrupt")
}

func TestCredentialStorageUnavailableUnderTestsWithoutOverride(t *testing.T) {
	// No SetCredentialsPathForTests: the developer's real login must never
	// be read or written by the test suite.
	_, err := Load()
	require.ErrorIs(t, err, ErrNotLoggedIn)
	assert.False(t, LoggedIn())

	removed, err := Logout()
	require.NoError(t, err)
	assert.False(t, removed)

	require.Error(t, save(&Credentials{AccessToken: "x"}))
}

func TestAccessToken_ValidTokenIsReturnedWithoutRefresh(t *testing.T) {
	useTempCredentials(t)
	writeCredentials(t, &Credentials{
		AccessToken: "still-valid",
		ExpiresAt:   time.Now().Add(time.Hour),
	})

	token, err := AccessToken(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "still-valid", token)
}

func TestAccessToken_NotLoggedIn(t *testing.T) {
	useTempCredentials(t)

	_, err := AccessToken(t.Context())
	require.ErrorIs(t, err, ErrNotLoggedIn)
}

func TestAccessToken_RefreshesExpiredToken(t *testing.T) {
	useTempCredentials(t)

	newAccess := makeJWT(t, chatgptClaims("acc_9", "pro"))
	var gotRequest map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/oauth/token", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotRequest))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  newAccess,
			"refresh_token": "refresh-2",
			"expires_in":    3600,
		})
	}))
	defer server.Close()
	restore := setAuthBaseURLForTests(server.URL)
	defer restore()

	writeCredentials(t, &Credentials{
		AccessToken:  "expired",
		RefreshToken: "refresh-1",
		Plan:         "plus",
		ExpiresAt:    time.Now().Add(-time.Minute),
	})

	token, err := AccessToken(t.Context())
	require.NoError(t, err)
	assert.Equal(t, newAccess, token)

	assert.Equal(t, "refresh_token", gotRequest["grant_type"])
	assert.Equal(t, "refresh-1", gotRequest["refresh_token"])
	assert.Equal(t, clientID, gotRequest["client_id"])

	// The refreshed credentials are persisted with rotated tokens and
	// identity re-derived from the fresh access token.
	stored, err := Load()
	require.NoError(t, err)
	assert.Equal(t, newAccess, stored.AccessToken)
	assert.Equal(t, "refresh-2", stored.RefreshToken)
	assert.Equal(t, "acc_9", stored.AccountID)
	assert.Equal(t, "pro", stored.Plan)
	assert.WithinDuration(t, time.Now().Add(time.Hour), stored.ExpiresAt, 5*time.Second)
}

func TestAccessToken_RefreshRejectedSuggestsSigningInAgain(t *testing.T) {
	useTempCredentials(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer server.Close()
	restore := setAuthBaseURLForTests(server.URL)
	defer restore()

	writeCredentials(t, &Credentials{
		AccessToken:  "expired",
		RefreshToken: "revoked",
		ExpiresAt:    time.Now().Add(-time.Minute),
	})

	_, err := AccessToken(t.Context())
	require.ErrorContains(t, err, invocation.DockerAgent()+" setup")
}

func TestAccessToken_ExpiredWithoutRefreshToken(t *testing.T) {
	useTempCredentials(t)
	writeCredentials(t, &Credentials{
		AccessToken: "expired",
		ExpiresAt:   time.Now().Add(-time.Minute),
	})

	_, err := AccessToken(t.Context())
	require.ErrorIs(t, err, ErrNotLoggedIn)
}

func TestExpiryFromToken(t *testing.T) {
	fromExpiresIn := expiryFromToken(tokenResponse{AccessToken: "x", ExpiresIn: 60})
	assert.WithinDuration(t, time.Now().Add(time.Minute), fromExpiresIn, 5*time.Second)

	exp := time.Now().Add(30 * time.Minute)
	jwtToken := makeJWT(t, map[string]any{"exp": exp.Unix()})
	fromClaim := expiryFromToken(tokenResponse{AccessToken: jwtToken})
	assert.WithinDuration(t, exp, fromClaim, 5*time.Second)

	assert.True(t, expiryFromToken(tokenResponse{AccessToken: "opaque"}).IsZero())
}

func TestCredentialsExpired(t *testing.T) {
	assert.False(t, (&Credentials{}).Expired(), "unknown expiry is not treated as expired")
	assert.False(t, (&Credentials{ExpiresAt: time.Now().Add(time.Hour)}).Expired())
	assert.True(t, (&Credentials{ExpiresAt: time.Now().Add(time.Minute)}).Expired(), "tokens about to expire refresh early")
	assert.True(t, (&Credentials{ExpiresAt: time.Now().Add(-time.Hour)}).Expired())
}
