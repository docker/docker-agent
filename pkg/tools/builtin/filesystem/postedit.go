//go:build !js

package filesystem

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/docker/docker-agent/pkg/shellpath"
)

// runPostEditCommands executes configured shell commands after a file edit.
func runPostEditCommands(ctx context.Context, workingDir string, postEditCommands []PostEditConfig, filePath string) error {
	var relPath string
	if workingDir != "" {
		rel, err := filepath.Rel(workingDir, filePath)
		if err == nil {
			relPath = filepath.ToSlash(rel)
		} else {
			slog.DebugContext(ctx, "Failed to resolve relative path for post-edit pattern", "workingDir", workingDir, "filePath", filePath, "error", err)
			relPath = filepath.ToSlash(filePath)
		}
	} else {
		relPath = filepath.ToSlash(filePath)
	}

	for _, postEdit := range postEditCommands {
		if !matchPostEdit(ctx, postEdit.Path, relPath, filePath) {
			continue
		}

		absolutePath, err := filepath.Abs(filePath)
		if err != nil {
			return fmt.Errorf("resolving post-edit file path: %w", err)
		}
		shell, argsPrefix := shellpath.DetectShell()
		cmd := exec.CommandContext(ctx, shell, append(argsPrefix, postEdit.Cmd)...)
		cmd.Dir = workingDir
		cmd.Env = cmd.Environ()
		cmd.Env = append(cmd.Env, "file="+absolutePath)

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("post-edit command failed for %s: %w", filePath, err)
		}
	}
	return nil
}

func matchPostEdit(ctx context.Context, patternStr, relPath, filePath string) bool {
	pattern := filepath.ToSlash(patternStr)
	target := filepath.Base(filePath)
	if strings.Contains(pattern, "/") {
		target = relPath
	}
	matched, err := path.Match(pattern, target)
	if err != nil {
		slog.WarnContext(ctx, "Invalid post-edit pattern", "pattern", patternStr, "error", err)
		return false
	}
	return matched
}
