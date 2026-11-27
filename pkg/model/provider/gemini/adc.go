//go:build !js

package gemini

import (
	"context"
	"fmt"

	"golang.org/x/oauth2/google"

	"github.com/docker/docker-agent/pkg/model/provider/options"
)

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// adcSupported reports whether the Vertex AI backend may fall back to
// application default credentials when no token source is given.
const adcSupported = true

func defaultADCTokenSource(ctx context.Context) (options.TokenSource, error) {
	creds, err := google.FindDefaultCredentials(ctx, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("finding Google default credentials: %w", err)
	}
	return func(context.Context) (string, error) {
		tok, err := creds.TokenSource.Token()
		if err != nil {
			return "", fmt.Errorf("getting Google access token: %w", err)
		}
		return tok.AccessToken, nil
	}, nil
}
