package evaluation

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
)

type progressGateEnvironment struct {
	entered chan struct{}
	release chan struct{}
}

func (e *progressGateEnvironment) Get(_ context.Context, name string) (string, bool) {
	if name == "PROGRESS_TEST_BARRIER" {
		close(e.entered)
		<-e.release
	}
	return "", false
}

type progressLockedWriter struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (w *progressLockedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.Write(data)
}

func (w *progressLockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

func TestRunnerProgressCancellationWaitsForWorker(t *testing.T) {
	t.Parallel()
	for _, isTTY := range []bool{false, true} {
		t.Run(fmt.Sprintf("tty=%t", isTTY), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "eval.json"), []byte(`{}`), 0o600))
				env := &progressGateEnvironment{entered: make(chan struct{}), release: make(chan struct{})}
				runner := newRunner(config.NewFileSource(filepath.Join(dir, "agent.yaml")),
					&config.RuntimeConfig{EnvProviderOverride: env},
					Config{EvalsDir: dir, Concurrency: 1, EnvVars: []string{"PROGRESS_TEST_BARRIER"}, ContainerRuntime: filepath.Join(dir, "missing-runtime")})
				runner.imageCache[imageKey{}] = "cached-image"
				release := sync.OnceFunc(func() { close(env.release) })
				defer release()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var out, terminal progressLockedWriter
				done := make(chan struct{})
				var runErr error
				var results []Result
				go func() {
					defer close(done)
					results, runErr = runner.Run(ctx, &terminal, &out, isTTY)
				}()
				<-env.entered
				cancel()
				synctest.Wait()
				// TTY refreshes may show 0/1, but only the final render adds a newline.
				assert.NotContains(t, terminal.String(), "\n")
				assert.NotContains(t, out.String(), "(0/1)", "parent cancellation must not trigger a premature final render")
				select {
				case <-done:
					t.Fatal("Run returned before its worker stopped")
				default:
				}

				release()
				<-done
				require.ErrorIs(t, runErr, context.Canceled)
				require.Len(t, results, 1)
				require.NotEmpty(t, results[0].Error)
				output := out.String()
				if isTTY {
					assert.NotContains(t, output, "(1/1)")
					output = terminal.String()
					assert.Equal(t, 1, strings.Count(output, "\n"))
				} else {
					assert.Empty(t, terminal.String())
					assert.Less(t, strings.Index(output, "starting "), strings.LastIndex(output, "100% (1/1)"), "the worker error must precede final progress")
				}
				assert.Contains(t, output, "100% (1/1)")
				assert.Equal(t, 1, strings.Count(output, "100% (1/1)"))
			})
		})
	}
}

type progressFinalWriter struct {
	bytes.Buffer

	entered chan struct{}
	release chan struct{}
}

func (w *progressFinalWriter) Write(data []byte) (int, error) {
	if strings.Contains(string(data), "(0/0)") {
		close(w.entered)
		<-w.release
	}
	return w.Buffer.Write(data)
}

func TestRunnerProgressJoinsFinalWrite(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		runner := newRunner(nil, nil, Config{EvalsDir: t.TempDir(), Concurrency: 1})
		out := &progressFinalWriter{entered: make(chan struct{}), release: make(chan struct{})}
		release := sync.OnceFunc(func() { close(out.release) })
		defer release()
		done := make(chan struct{})
		var runErr error
		go func() {
			defer close(done)
			_, runErr = runner.Run(t.Context(), io.Discard, out, false)
		}()
		<-out.entered
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("Run returned before the final write completed")
		default:
		}
		release()
		<-done
		require.NoError(t, runErr)
		assert.True(t, strings.HasSuffix(out.String(), "\n"))
		assert.Equal(t, 1, strings.Count(out.String(), "(0/0)"))
	})
}
