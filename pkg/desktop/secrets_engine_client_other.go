//go:build !darwin && !linux && !windows

package desktop

import (
	"errors"

	"github.com/docker/secrets-engine/client/dockerhub"
)

var errNoSecretsEngine = errors.New("the secrets engine is not available on this platform")

// newSecretsEngineHubAuth always fails: the SDK doesn't support this platform.
func newSecretsEngineHubAuth() (dockerhub.ClientAuth, error) {
	return nil, errNoSecretsEngine
}

func secretsEngineUnavailable(err error) bool {
	return errors.Is(err, errNoSecretsEngine)
}
