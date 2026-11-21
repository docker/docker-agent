//go:build !js

package mcp

import (
	"context"

	"github.com/docker/docker-agent/pkg/toolinstall"
)

func ensureCommand(ctx context.Context, command, version string) (string, error) {
	return toolinstall.EnsureCommand(ctx, command, version)
}

func prependBinDirToEnv(env []string) []string {
	return toolinstall.PrependBinDirToEnv(env)
}
