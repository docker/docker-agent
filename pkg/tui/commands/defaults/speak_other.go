//go:build !darwin

package defaults

import "github.com/docker/docker-agent/pkg/tui/commands"

func speakCommand() *commands.Item {
	return nil
}
