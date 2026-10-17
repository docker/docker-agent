//go:build darwin || linux || windows

package desktop

import (
	"bytes"
	"errors"
	"log/slog"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"

	secretsengine "github.com/docker/secrets-engine/client"
	"github.com/docker/secrets-engine/x/api"
	"github.com/docker/secrets-engine/x/secrets"
	"github.com/stretchr/testify/assert"
)

func sdkDialError(reason secretsengine.ConnectReason, err error) error {
	return &secretsengine.ConnectError{
		Reason:     reason,
		SocketPath: api.DesktopSocketPath(),
		Err:        &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", err)},
	}
}

func TestSecretsEngineFailureLogging(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		err  error
		warn bool
	}{
		{name: "a missing engine", err: sdkDialError(secretsengine.ReasonNotRunning, syscall.ENOENT)},
		{name: "permission denied", err: sdkDialError(secretsengine.ReasonPermissionDenied, syscall.EACCES), warn: true},
		{name: "a dial timeout", err: sdkDialError(secretsengine.ReasonTimeout, os.ErrDeadlineExceeded), warn: true},
		{name: "an unknown dial failure", err: sdkDialError(secretsengine.ReasonUnknown, syscall.ENOTSOCK), warn: true},
		{name: "access denied by the engine", err: secrets.ErrAccessDenied, warn: true},
	} {
		t.Run(tt.name+" is logged at the right level", func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				engine, logs := newLoggingEngine(t, &fakeEngine{err: tt.err})

				_, _ = engine.fetch(t.Context())
				if tt.warn {
					assert.Equal(t, 1, countFailureLogs(logs, "WARN"))
				} else {
					assert.Equal(t, 0, countFailureLogs(logs, "WARN"))
					assert.Equal(t, 1, countFailureLogs(logs, "DEBUG"))
				}

				if connErr, ok := errors.AsType[*secretsengine.ConnectError](tt.err); ok && connErr.Hint() != "" {
					assert.Contains(t, logs.String(), "hint=", "the SDK's hint is logged")
				} else {
					assert.NotContains(t, logs.String(), "hint=")
				}
			})
		})
	}

	t.Run("a missing Docker Desktop logs the SDK's hint", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			engine, logs := newLoggingEngine(t, &fakeEngine{err: sdkDialError(secretsengine.ReasonNotRunning, syscall.ENOENT)})

			_, _ = engine.fetch(t.Context())
			assert.Contains(t, logs.String(), `hint="Start Docker Desktop and retry`)
		})
	})

	t.Run("other failures are warned about once per run", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fake := &fakeEngine{err: secrets.ErrAccessDenied}
			engine, logs := newLoggingEngine(t, fake)

			lookUp := func() {
				endCooldown(engine)
				_, _ = engine.fetch(t.Context())
			}

			lookUp()
			lookUp()
			assert.Equal(t, 1, countFailureLogs(logs, "WARN"), "a failure that persists is warned about once")
			assert.Equal(t, 1, countFailureLogs(logs, "DEBUG"))

			fake.setErr(nil)
			lookUp()
			fake.setErr(secrets.ErrAccessDenied)
			lookUp()
			assert.Equal(t, 2, countFailureLogs(logs, "WARN"))
		})
	})
}

func newLoggingEngine(t *testing.T, fake *fakeEngine) (*secretsEngine, *bytes.Buffer) {
	t.Helper()

	var logs bytes.Buffer
	engine := newTestEngine(t, fake)
	engine.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return engine, &logs
}

func countFailureLogs(logs *bytes.Buffer, level string) int {
	n := 0
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "level="+level) && strings.Contains(line, `msg="`+secretsEngineFailureMsg+`"`) {
			n++
		}
	}
	return n
}
