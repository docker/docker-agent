package chatgpt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/docker/docker-agent/pkg/cli/invocation"
	"github.com/docker/docker-agent/pkg/httpclient"
)

// authBaseURL is the OAuth issuer. Variable so tests can point the flow at a
// local httptest server.
var authBaseURL = "https://auth.openai.com"

// tokenResponse is the OAuth token endpoint response shape, shared by the
// authorization-code exchange and the refresh grant.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
}

// AccessToken returns a valid access token for the ChatGPT Codex backend,
// transparently refreshing (and persisting) it when it is expired or close
// to expiry. It returns ErrNotLoggedIn when no login is stored.
func AccessToken(ctx context.Context) (string, error) {
	storeMu.Lock()
	defer storeMu.Unlock()

	creds, err := load()
	if err != nil {
		return "", err
	}
	if !creds.Expired() {
		return creds.AccessToken, nil
	}
	if creds.RefreshToken == "" {
		return "", fmt.Errorf("the stored ChatGPT access token expired and no refresh token is available: %w", ErrNotLoggedIn)
	}

	refreshed, err := refreshCredentials(ctx, creds)
	if err != nil {
		return "", err
	}
	// A failed persist (e.g. a read-only config dir inside a sandbox) must
	// not fail the request: the refreshed token is valid for this process.
	if err := save(refreshed); err != nil {
		slog.WarnContext(ctx, "Failed to persist refreshed ChatGPT credentials", "error", err)
	}
	return refreshed.AccessToken, nil
}

// refreshCredentials exchanges the refresh token for fresh tokens. The
// request body is JSON, matching what Codex CLI sends to the same endpoint.
func refreshCredentials(ctx context.Context, creds *Credentials) (*Credentials, error) {
	body, err := json.Marshal(map[string]string{
		"client_id":     clientID,
		"grant_type":    "refresh_token",
		"refresh_token": creds.RefreshToken,
		"scope":         "openid profile email",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to encode refresh request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authBaseURL+"/oauth/token", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpclient.NewHTTPClient(ctx).Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to refresh the ChatGPT access token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("the ChatGPT session is no longer valid (HTTP %d: %s); sign in again with `%s setup`",
				resp.StatusCode, strings.TrimSpace(string(detail)), invocation.DockerAgent())
		}
		return nil, fmt.Errorf("failed to refresh the ChatGPT access token: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}

	var token tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return nil, fmt.Errorf("failed to decode refresh response: %w", err)
	}
	if token.AccessToken == "" {
		return nil, errors.New("refresh response contained no access token")
	}

	refreshed := *creds
	refreshed.AccessToken = token.AccessToken
	if token.RefreshToken != "" {
		refreshed.RefreshToken = token.RefreshToken
	}
	if token.IDToken != "" {
		refreshed.IDToken = token.IDToken
	}
	refreshed.ExpiresAt = expiryFromToken(token)
	refreshed.LastRefresh = time.Now()
	applyClaims(&refreshed)
	return &refreshed, nil
}

// expiryFromToken derives the access token expiry, preferring the token
// endpoint's expires_in and falling back to the JWT exp claim.
func expiryFromToken(token tokenResponse) time.Time {
	if token.ExpiresIn > 0 {
		return time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}
	if claims := parseClaims(token.AccessToken); !claims.ExpiresAt.IsZero() {
		return claims.ExpiresAt
	}
	return time.Time{}
}

// applyClaims updates the identity fields (account id, email, plan) from
// the current tokens. The access token is parsed first so the id_token,
// which carries the richer profile, wins when both declare a claim; existing
// values are kept when neither token carries the claim.
func applyClaims(creds *Credentials) {
	for _, token := range []string{creds.AccessToken, creds.IDToken} {
		claims := parseClaims(token)
		if claims.AccountID != "" {
			creds.AccountID = claims.AccountID
		}
		if claims.Email != "" {
			creds.Email = claims.Email
		}
		if claims.Plan != "" {
			creds.Plan = claims.Plan
		}
	}
}

// tokenClaims are the OpenAI-specific claims docker-agent cares about.
type tokenClaims struct {
	AccountID string
	Email     string
	Plan      string
	ExpiresAt time.Time
}

// openAIAuthClaim is the namespaced JWT claim OpenAI uses to carry ChatGPT
// account metadata in both access and id tokens.
const openAIAuthClaim = "https://api.openai.com/auth"

// parseClaims extracts OpenAI claims from a JWT without verifying its
// signature: the token was received over TLS from the issuer (or supplied by
// the user) and is only inspected locally, never trusted for authorization.
func parseClaims(token string) tokenClaims {
	var out tokenClaims
	if token == "" {
		return out
	}
	mapClaims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, mapClaims); err != nil {
		return out
	}
	if email, ok := mapClaims["email"].(string); ok {
		out.Email = email
	}
	if exp, err := mapClaims.GetExpirationTime(); err == nil && exp != nil {
		out.ExpiresAt = exp.Time
	}
	if auth, ok := mapClaims[openAIAuthClaim].(map[string]any); ok {
		if id, ok := auth["chatgpt_account_id"].(string); ok {
			out.AccountID = id
		}
		if plan, ok := auth["chatgpt_plan_type"].(string); ok {
			out.Plan = plan
		}
	}
	return out
}

// AccountIDFromToken returns the ChatGPT account id embedded in an access
// token, or an empty string when the token does not carry one. The Codex
// backend requires this id as a request header for workspace accounts.
func AccountIDFromToken(token string) string {
	return parseClaims(token).AccountID
}
