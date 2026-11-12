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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestProgressRunCancellation(t *testing.T) {
	t.Parallel()
	for _, isTTY := range []bool{false, true} {
		t.Run(fmt.Sprintf("tty=%t", isTTY), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				var out bytes.Buffer
				progress := newProgressBar(&out, &out, 0, 2, isTTY)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan struct{})
				go func() {
					defer close(done)
					progress.run(ctx)
				}()
				synctest.Wait()
				select {
				case <-done:
					t.Fatal("progress returned before cancellation")
				default:
				}

				time.Sleep(100 * time.Millisecond) //nolint:forbidigo // Advance virtual time to trigger the ticker.
				synctest.Wait()
				assert.Equal(t, isTTY, out.Len() != 0, "only TTY output is rendered periodically")

				progress.complete("first", true)
				progress.printResult(Result{Title: "first"})
				progress.complete("second", false)
				progress.printResult(Result{Title: "second", Error: "failed"})
				cancel()
				<-done

				output := out.String()
				assert.Contains(t, output, "100% (2/2)")
				assert.True(t, strings.HasSuffix(output, "\n"))
				assert.Less(t, strings.Index(output, "second"), strings.LastIndex(output, "100% (2/2)"))
				assert.Equal(t, 1, strings.Count(output, "100% (2/2)"))

				length := out.Len()
				time.Sleep(time.Second) //nolint:forbidigo // Advance virtual time to check for post-return writes.
				synctest.Wait()
				assert.Equal(t, length, out.Len(), "no writes after run returns")
			})
		})
	}
}

func TestProgressRunAlreadyCanceled(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	progress := newProgressBar(io.Discard, &out, 0, 0, false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	progress.run(ctx)

	assert.Contains(t, out.String(), "0% (0/0)")
	assert.Equal(t, 1, strings.Count(out.String(), "\n"))
}

func TestProgressRunIndependentLifetimes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var out bytes.Buffer
		progress := newProgressBar(io.Discard, &out, 0, 1, false)
		first, cancelFirst := context.WithCancel(t.Context())
		defer cancelFirst()
		second, cancelSecond := context.WithCancel(t.Context())
		defer cancelSecond()
		firstDone, secondDone := make(chan struct{}), make(chan struct{})
		go func() { defer close(firstDone); progress.run(first) }()
		go func() { defer close(secondDone); progress.run(second) }()
		synctest.Wait()

		cancelFirst()
		<-firstDone
		synctest.Wait()
		select {
		case <-secondDone:
			t.Fatal("canceling one run stopped another")
		default:
		}
		assert.Equal(t, 1, strings.Count(out.String(), "\n"))

		progress.complete("result", true)
		cancelSecond()
		<-secondDone
		assert.Contains(t, out.String(), "100% (1/1)")
		assert.Equal(t, 2, strings.Count(out.String(), "\n"))
	})
}

func TestProgressRunConcurrentResults(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var out bytes.Buffer
		const count = 20
		progress := newProgressBar(&out, &out, 0, count, true)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); progress.run(ctx) }()
		finishWork := make(chan struct{})
		var workers sync.WaitGroup
		for i := range count {
			workers.Go(func() {
				title := fmt.Sprintf("result-%d", i)
				progress.setRunning(title)
				<-finishWork
				progress.complete(title, true)
				progress.printResult(Result{Title: title})
			})
		}
		synctest.Wait()
		time.Sleep(100 * time.Millisecond) //nolint:forbidigo // Render while every worker is active in virtual time.
		synctest.Wait()
		close(finishWork)
		workers.Wait()
		cancel()
		<-done
		assert.Contains(t, out.String(), "100% (20/20)")
		assert.Equal(t, count, strings.Count(out.String(), "$0.000000"))
		assert.Equal(t, 1, strings.Count(out.String(), "100% (20/20)"))
	})
}

type progressResultWriter struct {
	bytes.Buffer

	onRunning func()
}

func (w *progressResultWriter) Write(data []byte) (int, error) {
	if strings.HasPrefix(string(data), "Running ") && w.onRunning != nil {
		w.onRunning()
	}
	return w.Buffer.Write(data)
}

func TestRunnerProgressFinalOutput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		count  int
		cancel bool
	}{
		{name: "empty"},
		{name: "worker errors", count: 2},
		{name: "cancellation", count: 2, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for i := range tc.count {
				require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("eval-%d.json", i)), []byte(`{}`), 0o600))
			}
			runner := newRunner(config.NewFileSource(filepath.Join(dir, "agent.yaml")),
				&config.RuntimeConfig{EnvProviderForTests: environment.NewNoEnvProvider()},
				Config{EvalsDir: dir, Concurrency: 2, ContainerRuntime: filepath.Join(dir, "missing-runtime")})
			// Exercise the real worker path without building or running containers.
			runner.imageCache[imageKey{}] = "cached-image"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var out progressResultWriter
			if tc.cancel {
				out.onRunning = cancel
			}

			results, err := runner.Run(ctx, io.Discard, &out, false)

			if tc.cancel {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, results, tc.count)
			completed := tc.count
			if tc.cancel {
				completed = 0
			}
			final := fmt.Sprintf("(%d/%d)", completed, tc.count)
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			assert.Contains(t, lines[len(lines)-1], final, "final progress must be written before Run returns")
			for _, result := range results {
				if !tc.cancel {
					assert.NotEmpty(t, result.Error)
				}
			}
		})
	}
}
