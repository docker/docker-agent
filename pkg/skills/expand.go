package skills

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/shellpath"
)

// commandTimeout is the maximum time allowed for a single command expansion.
const commandTimeout = 30 * time.Second

// maxOutputSize is the maximum number of bytes read from a command's stdout.
const maxOutputSize = 1 << 20 // 1 MB

// commandPattern matches the !`command` syntax used by Claude Code skills
// to embed dynamic command output into skill content.
var commandPattern = regexp.MustCompile("!`([^`]+)`")

// ExpandCommands replaces all !`command` patterns in the given content
// with the stdout of executing each command via the system shell.
// Commands are executed with the specified working directory.
// If a command fails, the pattern is replaced with an error message
// rather than failing the entire expansion.
func ExpandCommands(ctx context.Context, content, workDir string) string {
	return commandPattern.ReplaceAllStringFunc(content, func(match string) string {
		command := match[2 : len(match)-1] // strip leading !` and trailing `

		output, err := runCommand(ctx, command, workDir)
		if err != nil {
			slog.WarnContext(ctx, "Skill command expansion failed", "command", command, "error", err)
			return fmt.Sprintf("[error executing `%s`: %s]", command, err)
		}

		return strings.TrimRight(output, "\n")
	})
}

// runCommand executes a shell command and returns its stdout (up to maxOutputSize bytes).
// The command runs in the specified working directory.
func runCommand(ctx context.Context, command, workDir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	shell, argsPrefix := shellpath.DetectShell()
	cmd := exec.CommandContext(ctx, shell, append(argsPrefix, command)...)
	cmd.Dir = workDir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}

	if err := cmd.Start(); err != nil {
		return "", err
	}

	out, err := io.ReadAll(io.LimitReader(stdout, maxOutputSize))
	if err != nil {
		return "", err
	}

	// Drain any remaining stdout so the process doesn't block on a full pipe
	// and hang until the context timeout kills it.
	_, _ = io.Copy(io.Discard, stdout)

	if err := cmd.Wait(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("command timed out after %s", commandTimeout)
		}
		if stderrMsg := strings.TrimSpace(stderr.String()); stderrMsg != "" {
			return "", fmt.Errorf("%w: %s", err, stderrMsg)
		}
		return "", err
	}

	return string(out), nil
}
