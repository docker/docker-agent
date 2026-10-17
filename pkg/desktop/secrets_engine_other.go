//go:build !darwin && !linux && !windows

package desktop

import "context"

// secretsEngineToken never finds a token: the SDK doesn't support this platform.
func secretsEngineToken(context.Context) (string, bool) {
	return "", false
}
