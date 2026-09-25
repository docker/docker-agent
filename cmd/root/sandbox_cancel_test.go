package root

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/sandbox"
)

func TestCloudCancellationCleansOwnedSandbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable shim")
	}
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })
	executable, err := os.Executable()
	require.NoError(t, err)
	fakeDir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestCloudCancellationHelper$ -- \"$@\"\n", executable)
	require.NoError(t, os.WriteFile(filepath.Join(fakeDir, "sbx"), []byte(script), 0o700))
	t.Setenv("PATH", fakeDir)
	t.Setenv("SANDBOX_CANCEL_HELPER", "1")
	for _, phase := range []string{"create-with-id", "create-without-id", "exec"} {
		t.Run(phase, func(t *testing.T) {
			t.Setenv("SANDBOX_CANCEL_PHASE", phase)
			state := filepath.Join(t.TempDir(), "state")
			t.Setenv("SANDBOX_CANCEL_STATE", state)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cmd := newRunCmd()
			require.NoError(t, cmd.ParseFlags([]string{"--model", "openai/gpt-5.6"}))
			cmd.SetOut(io.Discard)
			cmd.SetErr(&cancelWhenReady{cancel: cancel})
			err := runInCloudSandbox(ctx, cmd, []string{"default"}, sandbox.NewBackend(true, true), sandbox.Options{Cloud: true, TTL: time.Hour})
			var status cli.StatusError
			require.ErrorAs(t, err, &status)
			assert.Equal(t, 130, status.StatusCode)
			stopped, err := os.ReadFile(state + ".stopped")
			require.NoError(t, err)
			assert.Equal(t, "sbx_owned123", string(stopped))
		})
	}
}

type cancelWhenReady struct {
	cancel context.CancelFunc
	once   sync.Once
}

func (w *cancelWhenReady) Write(data []byte) (int, error) {
	if strings.Contains(string(data), "ready-to-cancel") {
		w.once.Do(w.cancel)
	}
	return len(data), nil
}

func TestCloudCancellationHelper(*testing.T) {
	if os.Getenv("SANDBOX_CANCEL_HELPER") != "1" {
		return
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	state, phase := os.Getenv("SANDBOX_CANCEL_STATE"), os.Getenv("SANDBOX_CANCEL_PHASE")
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	switch args[1] {
	case "run":
		name := args[slices.Index(args, "--name")+1]
		data, _ := json.Marshal(map[string]any{"sandboxes": []map[string]string{
			{"name": "someone-else", "id": "sbx_unrelated"},
			{"name": name, "id": "sbx_owned123"},
		}})
		_ = os.WriteFile(state, data, 0o600)
		if phase != "create-without-id" {
			_, _ = os.Stdout.WriteString("sbx_owned123\n")
		}
		if phase == "exec" {
			os.Exit(0)
		}
	case "ls":
		data, _ := os.ReadFile(state)
		_, _ = os.Stdout.Write(data)
		os.Exit(0)
	case "stop":
		if phase == "exec" {
			if _, err := os.Stat(state + ".exec-cleaned"); err != nil {
				os.Exit(98)
			}
		}
		_ = os.WriteFile(state+".stopped", []byte(args[2]), 0o600)
		os.Exit(0)
	case "exec":
	default:
		os.Exit(99)
	}
	_, _ = os.Stderr.WriteString("ready-to-cancel\n")
	<-interrupt
	if args[1] == "exec" {
		_ = os.WriteFile(state+".exec-cleaned", nil, 0o600)
	}
	os.Exit(130)
}
