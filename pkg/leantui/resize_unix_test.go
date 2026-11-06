//go:build !windows

package leantui

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/muesli/cancelreader"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/term"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/session"
)

type resizeOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (o *resizeOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buffer.Write(p)
}

func (o *resizeOutput) hasSeparator(width int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return regexp.MustCompile(`(^|[^─])` + strings.Repeat("─", width) + `[^─]`).Match(o.buffer.Bytes())
}

func TestRunPollsResizeWhileIdle(t *testing.T) {
	t.Parallel()

	master, tty, err := pty.Open()
	require.NoError(t, err)
	t.Cleanup(func() { _ = master.Close(); _ = tty.Close() })
	require.NoError(t, pty.Setsize(master, &pty.Winsize{Cols: 80, Rows: 24}))
	initialState, err := term.GetState(int(tty.Fd()))
	require.NoError(t, err)

	cancelRead, cancelWrite, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = cancelRead.Close(); _ = cancelWrite.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestResizeHelperProcess$")
	cmd.Env = append(os.Environ(), "DOCKER_AGENT_RESIZE_HELPER=1")
	cmd.Dir = t.TempDir()
	cmd.Stdin, cmd.Stdout = tty, tty
	cmd.ExtraFiles = []*os.File{cancelRead}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	reader, err := cancelreader.NewReader(master)
	require.NoError(t, err)
	var output resizeOutput
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(&output, reader)
	}()
	t.Cleanup(func() {
		reader.Cancel()
		<-drained
		require.NoError(t, reader.Close())
	})
	require.NoError(t, cmd.Start())
	waited := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(waited) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			t.Error("resize helper did not exit")
		}
	})

	require.Eventually(t, func() bool { return output.hasSeparator(80) }, 5*time.Second, 10*time.Millisecond)
	for _, width := range []uint16{100, 40, 120} {
		require.NoError(t, pty.Setsize(master, &pty.Winsize{Cols: width, Rows: 24}))
		require.Eventually(t, func() bool {
			return output.hasSeparator(int(width))
		}, 5*time.Second, 10*time.Millisecond, "idle UI did not redraw at width %d", width)
	}

	require.NoError(t, cancelWrite.Close())
	select {
	case <-waited:
		require.NoError(t, waitErr, stderr.String())
	case <-ctx.Done():
		t.Fatal("resize helper did not exit after cancellation")
	}
	restoredState, err := term.GetState(int(tty.Fd()))
	require.NoError(t, err)
	assert.Equal(t, initialState, restoredState)
}

func TestResizeHelperProcess(t *testing.T) {
	if os.Getenv("DOCKER_AGENT_RESIZE_HELPER") != "1" {
		return
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelRead := os.NewFile(3, "cancel")
	defer cancelRead.Close()
	go func() {
		_, _ = io.Copy(io.Discard, cancelRead)
		cancel()
	}()
	cfg := Config{
		App:        app.New(ctx, &cycleThinkingRuntime{}, session.New()),
		WorkingDir: ".",
		History:    &history.History{},
		ShowBanner: new(false),
	}
	require.NoError(t, Run(ctx, cfg))
}
