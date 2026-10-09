//go:build darwin || linux || windows

package desktop

import (
	"errors"
	"io/fs"

	secretsengine "github.com/docker/secrets-engine/client"
	"github.com/docker/secrets-engine/client/dockerhub"
	"github.com/docker/secrets-engine/x/api"
)

// newSecretsEngineHubAuth connects to Docker Desktop's engine socket, not the
// SDK's default standalone daemon socket.
func newSecretsEngineHubAuth() (dockerhub.ClientAuth, error) {
	engine, err := secretsengine.New(secretsengine.WithSocketPath(api.DesktopSocketPath()))
	if err != nil {
		return nil, err
	}
	return engine.HubAuth(secretsEngineHubOptions()...), nil
}

// secretsEngineUnavailable reports whether err means nothing is at the engine
// socket: Docker Desktop is not installed, not running, or predates the
// engine. The SDK reports every failed dial as unavailable, so tell those apart
// from an engine that is there but can't be reached, such as permission denied.
//
// TODO: use secretsengine.ErrSecretsEngineUnreachable once
// https://github.com/docker/secrets-engine/pull/677 is released.
func secretsEngineUnavailable(err error) bool {
	return errors.Is(err, secretsengine.ErrSecretsEngineNotAvailable) &&
		(errors.Is(err, fs.ErrNotExist) || errors.Is(err, errConnRefused))
}
