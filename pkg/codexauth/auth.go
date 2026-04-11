package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const authFileName = "auth.json"

// Auth represents the persisted local Codex CLI authentication state.
type Auth struct {
	OpenAIAPIKey string `json:"OPENAI_API_KEY"`
	AuthMode     string `json:"auth_mode"`
	Tokens       struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}

// Claims contains the subset of JWT claims used by docker-agent.
type Claims struct {
	ChatGPTAccountID string `json:"chatgpt_account_id"`
}

// DefaultPath returns the default location of the local Codex auth file.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", authFileName)
}

// Load reads the local Codex auth file from the default location.
func Load() (*Auth, error) {
	path := DefaultPath()
	if path == "" {
		return nil, errors.New("unable to resolve Codex auth path")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var auth Auth
	if err := json.Unmarshal(data, &auth); err != nil {
		return nil, err
	}
	return &auth, nil
}

// HasChatGPTAuth reports whether the auth file contains a ChatGPT-backed login.
func (a *Auth) HasChatGPTAuth() bool {
	return strings.EqualFold(a.AuthMode, "chatgpt") && a.Tokens.AccessToken != ""
}

// AccountID returns the ChatGPT account ID from the auth file or token claims.
func (a *Auth) AccountID() string {
	if a.Tokens.AccountID != "" {
		return a.Tokens.AccountID
	}

	for _, token := range []string{a.Tokens.IDToken, a.Tokens.AccessToken} {
		if claims, err := parseClaims(token); err == nil && claims.ChatGPTAccountID != "" {
			return claims.ChatGPTAccountID
		}
	}

	return ""
}

func parseClaims(jwt string) (*Claims, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return nil, errors.New("invalid JWT format")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}

	var raw struct {
		Auth Claims `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, err
	}
	return &raw.Auth, nil
}
