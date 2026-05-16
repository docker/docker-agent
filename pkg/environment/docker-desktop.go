package environment

import (
	"context"

	"github.com/docker/docker-agent/pkg/desktop"
)

const (
	DockerDesktopEmail    = "DOCKER_EMAIL"
	DockerDesktopUsername = "DOCKER_USERNAME"
	DockerDesktopTokenEnv = "DOCKER_TOKEN"
)

type DockerDesktopProvider struct{}

func NewDockerDesktopProvider() *DockerDesktopProvider {
	return &DockerDesktopProvider{}
}

func (p *DockerDesktopProvider) Get(ctx context.Context, name string) (string, bool) {
	switch name {
	case DockerDesktopEmail:
		return desktop.GetUserInfo(ctx).Email, true

	case DockerDesktopUsername:
		return desktop.GetUserInfo(ctx).Username, true

	case DockerDesktopTokenEnv:
		return desktop.GetToken(ctx), true

	default:
		return "", false
	}
}
