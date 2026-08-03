package invocation

import "github.com/docker/cli/cli-plugins/plugin"

// DockerAgent returns the command users can run for the current invocation mode.
func DockerAgent() string {
	if plugin.RunningStandalone() {
		return "docker-agent"
	}
	return "docker agent"
}
