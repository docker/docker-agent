package skills

import (
	"bytes"
	"context"
	"errors"
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

// Runner executes one command embedded in skill content and returns its
// stdout. A skill body is untrusted data, so implementations are expected to
// get the user's consent before running anything on their machine: returning
// an error keeps the command from running and inlines the reason in the
// expanded content.
type Runner func(ctx context.Context, command string) (string, error)

// ExpansionAbort marks an error that must stop command expansion rather than
// being embedded in the skill content.
type ExpansionAbort interface {
	error
	AbortExpansion()
}

// ExpandCommands replaces all !`command` patterns in content. Runner errors
// are embedded in the result; callers that need abort semantics should use
// [ExpandCommandsWithError].
func ExpandCommands(ctx context.Context, content string, run Runner) string {
	expanded, _ := ExpandCommandsWithError(ctx, content, run)
	return expanded
}

// ExpandCommandsWithError behaves like [ExpandCommands], but propagates an
// [ExpansionAbort] or context cancellation returned by the runner.
func ExpandCommandsWithError(ctx context.Context, content string, run Runner) (string, error) {
	matches := commandPattern.FindAllStringIndex(content, -1)
	if len(matches) == 0 {
		return content, nil
	}

	var expanded strings.Builder
	expanded.Grow(len(content))
	last := 0
	for _, match := range matches {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		expanded.WriteString(content[last:match[0]])
		placeholder := content[match[0]:match[1]]
		command := placeholder[2 : len(placeholder)-1]

		output, err := run(ctx, command)
		if err != nil {
			var abort ExpansionAbort
			if errors.As(err, &abort) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return "", err
			}
			slog.WarnContext(ctx, "Skill command expansion failed", "command", command, "error", err)
			fmt.Fprintf(&expanded, "[error executing `%s`: %s]", command, err)
		} else {
			expanded.WriteString(strings.TrimRight(output, "\n"))
		}
		last = match[1]
	}
	expanded.WriteString(content[last:])
	return expanded.String(), nil
}

// ShellRunner returns a [Runner] that executes commands with the system shell
// in workDir. It performs no approval of its own — wrap it to gate execution.
func ShellRunner(workDir string) Runner {
	return func(ctx context.Context, command string) (string, error) {
		return runCommand(ctx, command, workDir)
	}
}

// runCommand executes a shell command and returns its stdout (up to maxOutputSize bytes).
// The command runs in the specified working directory.
func runCommand(ctx context.Context, command, workDir string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	shell, argsPrefix := shellpath.DetectShell()
	cmd := exec.CommandContext(commandCtx, shell, append(argsPrefix, command)...)
	cmd.Dir = workDir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}

	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}

	out, err := io.ReadAll(io.LimitReader(stdout, maxOutputSize))
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}

	// Drain any remaining stdout so the process doesn't block on a full pipe
	// and hang until the context timeout kills it.
	_, _ = io.Copy(io.Discard, stdout)

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if commandCtx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("command timed out after %s", commandTimeout)
		}
		if stderrMsg := strings.TrimSpace(stderr.String()); stderrMsg != "" {
			return "", fmt.Errorf("%w: %s", err, stderrMsg)
		}
		return "", err
	}

	return string(out), nil
}
