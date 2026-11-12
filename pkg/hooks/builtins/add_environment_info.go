package builtins

import (
	"context"
	"runtime"

	"github.com/docker/docker-agent/pkg/hooks"
)

// AddEnvironmentInfo is the registered name of the add_environment_info builtin.
const AddEnvironmentInfo = "add_environment_info"

// addEnvironmentInfo emits cwd/git/OS/arch/shell as session_start context.
// No-op when Cwd is empty.
func addEnvironmentInfo(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
	if in == nil || in.Cwd == "" {
		return nil, nil
	}
	return hooks.NewAdditionalContextOutput(hooks.EventSessionStart, environmentInfo(in.Cwd)), nil
}

// displayOS returns a friendlier label for the common values of
// runtime.GOOS, falling back to GOOS itself for anything exotic.
func displayOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	default:
		return runtime.GOOS
	}
}

// displayArch maps amd64 to its more familiar "x64" alias and passes
// every other value of runtime.GOARCH through unchanged.
func displayArch() string {
	if runtime.GOARCH == "amd64" {
		return "x64"
	}
	return runtime.GOARCH
}
