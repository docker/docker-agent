package environment

import (
	"context"
	"net/url"
	"strings"

	"github.com/docker/docker-agent/pkg/desktop"
)

const (
	DockerDesktopEmail    = "DOCKER_EMAIL"
	DockerDesktopUsername = "DOCKER_USERNAME"
	DockerDesktopTokenEnv = "DOCKER_TOKEN"
)

// IsDockerDomainURL reports whether rawURL targets docker.com or one of its
// subdomains over HTTPS.
func IsDockerDomainURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "docker.com" || strings.HasSuffix(host, ".docker.com")
}

// IsTrustedDockerURL checks if the URL targets a domain trusted to receive
// the Docker Desktop JWT. It matches:
//   - "docker.com" and any subdomain (e.g. "desktop.docker.com") over HTTPS only
//   - localhost addresses ("localhost", "127.0.0.1", "::1") over HTTP or HTTPS
//
// It performs strict hostname and scheme validation to prevent token leakage.
func IsTrustedDockerURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	return IsDockerDomainURL(rawURL)
}

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
