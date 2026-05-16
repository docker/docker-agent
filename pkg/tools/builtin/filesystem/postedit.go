//go:build !js

package filesystem

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"

	"github.com/docker/docker-agent/pkg/shellpath"
)

// runPostEditCommands executes configured shell commands after a file edit.
func runPostEditCommands(ctx context.Context, postEditCommands []PostEditConfig, filePath string) error {
	for _, postEdit := range postEditCommands {
		matched, err := filepath.Match(postEdit.Path, filepath.Base(filePath))
		if err != nil {
			slog.WarnContext(ctx, "Invalid post-edit pattern", "pattern", postEdit.Path, "error", err)
			continue
		}
		if !matched {
			continue
		}

		shell, argsPrefix := shellpath.DetectShell()
		cmd := exec.CommandContext(ctx, shell, append(argsPrefix, postEdit.Cmd)...)
		cmd.Env = cmd.Environ()
		cmd.Env = append(cmd.Env, "file="+filePath)

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("post-edit command failed for %s: %w", filePath, err)
		}
	}
	return nil
}
