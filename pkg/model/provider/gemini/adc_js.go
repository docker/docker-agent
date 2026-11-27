//go:build js

package gemini

import (
	"context"
	"errors"

	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// The browser has no credential files or metadata server to discover, so a
// Vertex AI client without an explicit token source fails closed instead of
// probing for application default credentials.
const adcSupported = false

func defaultADCTokenSource(context.Context) (options.TokenSource, error) {
	return nil, errors.New("vertex AI requires an explicit token source on this platform: application default credentials are unavailable")
}
