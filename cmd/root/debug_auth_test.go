package root

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildTestJWT(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, _ := json.Marshal(claims)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := base64.RawURLEncoding.EncodeToString([]byte("fakesig"))
	return fmt.Sprintf("%s.%s.%s", header, payloadB64, sig)
}

func TestParseAuthInfo_ValidToken(t *testing.T) {
	t.Parallel()

	now := time.Now()
	exp := now.Add(time.Hour)
	token := buildTestJWT(map[string]any{
		"sub": "user-123",
		"iss": "docker",
		"iat": now.Unix(),
		"exp": exp.Unix(),
	})

	info, err := parseAuthInfo(token)
	require.NoError(t, err)
	assert.Equal(t, token, info.Token)
	assert.Equal(t, "user-123", info.Subject)
	assert.Equal(t, "docker", info.Issuer)
	assert.False(t, info.Expired)
	assert.WithinDuration(t, now, info.IssuedAt, time.Second)
	assert.WithinDuration(t, exp, info.ExpiresAt, time.Second)
}

func TestParseAuthInfo_ExpiredToken(t *testing.T) {
	t.Parallel()

	exp := time.Now().Add(-time.Hour)
	token := buildTestJWT(map[string]any{
		"sub": "user-456",
		"exp": exp.Unix(),
	})

	info, err := parseAuthInfo(token)
	require.NoError(t, err)
	assert.True(t, info.Expired)
	assert.Equal(t, "user-456", info.Subject)
}

func TestParseAuthInfo_InvalidToken(t *testing.T) {
	t.Parallel()

	_, err := parseAuthInfo("not-a-jwt")
	require.Error(t, err)
}

func TestPrintAuthInfoText(t *testing.T) {
	t.Parallel()

	info := &authInfo{
		Token:     "eyJhbGciOiJIUzI1NiJ9.xxxxxxxxxxxx.yyyyyyyy1234567890",
		Username:  "testuser",
		Email:     "test@example.com",
		Subject:   "sub-123",
		Issuer:    "docker",
		IssuedAt:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
		Expired:   false,
	}

	var buf bytes.Buffer
	printAuthInfoText(&buf, info)

	output := buf.String()
	assert.Contains(t, output, "testuser")
	assert.Contains(t, output, "test@example.com")
	assert.Contains(t, output, "sub-123")
	assert.Contains(t, output, "docker")
	assert.Contains(t, output, "✅ Valid")
}

func TestPrintAuthInfoText_Expired(t *testing.T) {
	t.Parallel()

	info := &authInfo{
		Token:     "eyJhbGciOiJIUzI1NiJ9.xxxxxxxxxxxx.yyyyyyyy1234567890",
		ExpiresAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		Expired:   true,
	}

	var buf bytes.Buffer
	printAuthInfoText(&buf, info)

	assert.Contains(t, buf.String(), "❌ Expired")
}
