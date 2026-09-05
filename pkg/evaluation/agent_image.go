package evaluation

import (
	"fmt"

	"github.com/Masterminds/semver/v3"

	"github.com/docker/docker-agent/pkg/version"
)

// defaultAgentImageRepo is the Docker Hub repository CI publishes the
// docker-agent binary to, tagged both with release semvers and a rolling
// :edge tracking main.
const defaultAgentImageRepo = "docker/docker-agent"

// edgeAgentImage is injected when the host CLI isn't a release build (e.g.
// compiled from main or a PR), so there is no matching version tag to pin to.
const edgeAgentImage = defaultAgentImageRepo + ":edge"

// NoAgentImage is the Config.AgentImage sentinel that skips injecting a
// docker-agent binary into the eval image entirely, trusting whatever
// /docker-agent is already present in the (custom) base image.
const NoAgentImage = "none"

// DefaultAgentImage returns the docker-agent image injected into eval
// containers when no explicit --agent-image override is given: the release
// image matching the host CLI's own version, so eval results are
// reproducible against a pinned build rather than a moving main-HEAD binary.
// Falls back to the rolling :edge image when the host binary isn't a release
// build (version.Version isn't a valid semantic version, e.g. "dev" or "pr").
func DefaultAgentImage() string {
	v, err := semver.NewVersion(version.Version)
	if err != nil {
		return edgeAgentImage
	}
	return fmt.Sprintf("%s:%s", defaultAgentImageRepo, v.String())
}

// ResolvedAgentImage returns the docker-agent image to inject into eval
// containers for the given config: the explicit cfg.AgentImage override when
// set, DefaultAgentImage() when unset, or "" when cfg.AgentImage is
// NoAgentImage, meaning injection is skipped and the base image's own
// /docker-agent binary is trusted instead.
func ResolvedAgentImage(cfg Config) string {
	switch cfg.AgentImage {
	case "":
		return DefaultAgentImage()
	case NoAgentImage:
		return ""
	default:
		return cfg.AgentImage
	}
}
