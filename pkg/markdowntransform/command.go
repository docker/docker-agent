package markdowntransform

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/shellpath"
)

const maxLineBytes = 16 << 20

// command is a running process exposed as a line stream. It knows nothing
// about the protocol spoken over its pipes.
type command struct {
	stdin io.WriteCloser
	lines <-chan []byte // closed when stdout closes
	stop  context.CancelFunc
}

// startCommand runs config.Command under the user's shell until ctx is done
// or Close is called. Stderr goes to the debug log, never the terminal.
func startCommand(ctx context.Context, config Config) (*command, error) {
	ctx, cancel := context.WithCancel(ctx)
	shell, args := shellpath.DetectShell()
	cmd := exec.CommandContext(ctx, shell, append(args, config.Command)...)
	cmd.Dir = path.ExpandWorkingDir("markdown_transformers.working_dir", config.WorkingDir)
	cmd.Env = os.Environ()
	for name, value := range config.Env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	// Bounds Wait when descendants inherit the pipes.
	cmd.WaitDelay = time.Second
	cmd.Stderr = stderrLog{command: config.Command}
	stdoutReader, stdoutWriter := io.Pipe()
	cmd.Stdout = stdoutWriter
	stdin, err := cmd.StdinPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start %q: %w", config.Command, err)
	}

	go func() {
		_ = cmd.Wait()
		_ = stdoutWriter.Close()
	}()
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		defer stdoutReader.Close()
		scanner := bufio.NewScanner(stdoutReader)
		scanner.Buffer(nil, maxLineBytes)
		for scanner.Scan() {
			select {
			case lines <- bytes.Clone(scanner.Bytes()):
			case <-ctx.Done():
				return
			}
		}
	}()
	return &command{stdin: stdin, lines: lines, stop: cancel}, nil
}

func (c *command) Close() {
	c.stop()
	_ = c.stdin.Close()
}

type stderrLog struct{ command string }

func (l stderrLog) Write(data []byte) (int, error) {
	slog.Debug("Markdown transformer stderr", "command", l.command, "output", string(bytes.TrimSpace(data)))
	return len(data), nil
}
