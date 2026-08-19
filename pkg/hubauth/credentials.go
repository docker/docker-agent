package hubauth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/docker/cli/cli/config"
)

const (
	// indexServer is the credential store key `docker login` uses for Hub.
	indexServer = "https://index.docker.io/v1/"

	// tokenPrefix marks a stored secret as a Docker-issued access token
	// (dckr_pat_, dckr_oat_, ...). Anything else is an account password: 2FA
	// can make it unusable here and it is too sensitive to send around, so we
	// never exchange it.
	tokenPrefix = "dckr_"
)

// isAccessToken reports whether secret is a Docker access token rather than a
// password.
func isAccessToken(secret string) bool {
	return strings.HasPrefix(secret, tokenPrefix)
}

// dockerConfigCredentials reads the Hub credentials from the Docker CLI
// config, going through the configured credential helper when there is one.
//
// A helper that answers with an identity token instead of a password is of no
// use here: that token authenticates to the registry, not to Hub.
func dockerConfigCredentials() (username, secret string, err error) {
	cfg, err := config.Load(config.Dir())
	if err != nil {
		return "", "", fmt.Errorf("loading Docker CLI config: %w", err)
	}
	auth, err := cfg.GetAuthConfig(indexServer)
	if err != nil {
		return "", "", fmt.Errorf("reading Docker credentials: %w", err)
	}
	return auth.Username, auth.Password, nil
}

// fingerprint identifies a credential pair without keeping it in memory.
func fingerprint(username, secret string) string {
	sum := sha256.Sum256([]byte(username + "\x00" + secret))
	return hex.EncodeToString(sum[:])
}
