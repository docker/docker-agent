//go:build darwin || linux || windows

package desktop

import (
	"bytes"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	secretsengine "github.com/docker/secrets-engine/client"
	"github.com/docker/secrets-engine/x/secrets"
	"github.com/stretchr/testify/assert"
)

// sdkDialError is how the SDK reports a failed dial of the engine socket.
func sdkDialError(err error) error {
	dial := &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", err)}
	return fmt.Errorf("%w: %w", secretsengine.ErrSecretsEngineNotAvailable, dial)
}

func TestSecretsEngineUnavailable(t *testing.T) {
	for _, tt := range []struct {
		name        string
		err         error
		unavailable bool
	}{
		{name: "missing socket", err: sdkDialError(syscall.ENOENT), unavailable: true},
		{name: "nothing listening", err: sdkDialError(errConnRefused), unavailable: true},
		{name: "permission denied", err: sdkDialError(syscall.EACCES)},
		{name: "not a socket", err: sdkDialError(syscall.ENOTSOCK)},
		{
			name: "dial timeout",
			err: fmt.Errorf("%w: %w", secretsengine.ErrSecretsEngineNotAvailable,
				&net.OpError{Op: "dial", Net: "unix", Err: os.ErrDeadlineExceeded}),
		},
		{name: "access denied by the engine", err: secrets.ErrAccessDenied},
		{name: "missing file outside a dial", err: fs.ErrNotExist},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.unavailable, secretsEngineUnavailable(fmt.Errorf("retrieve default account metadata: %w", tt.err)))
		})
	}
}

func TestSecretsEngineFailureLogging(t *testing.T) {
	t.Run("a missing engine is only logged at debug level", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := captureLogs(t)
			installFakeBackend(t, &fakeBackend{token: makeToken(t, time.Now().Add(time.Hour))})
			installFakeEngine(t, &fakeEngine{err: sdkDialError(syscall.ENOENT)})

			GetToken(t.Context())
			assert.Equal(t, 0, countFailureLogs(logs, "WARN"))
			assert.Equal(t, 1, countFailureLogs(logs, "DEBUG"))
		})
	})

	t.Run("an engine that can't be reached is warned about", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := captureLogs(t)
			installFakeBackend(t, &fakeBackend{token: makeToken(t, time.Now().Add(time.Hour))})
			installFakeEngine(t, &fakeEngine{err: sdkDialError(syscall.EACCES)})

			GetToken(t.Context())
			assert.Equal(t, 1, countFailureLogs(logs, "WARN"))
		})
	})

	t.Run("other failures are warned about once per run", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := captureLogs(t)
			installFakeBackend(t, &fakeBackend{token: makeToken(t, time.Now().Add(time.Hour))})
			engine := &fakeEngine{err: secrets.ErrAccessDenied}
			installFakeEngine(t, engine)

			lookUp := func() {
				endSecretsEngineCooldown()
				expireCache()
				GetToken(t.Context())
			}

			lookUp()
			lookUp()
			assert.Equal(t, 1, countFailureLogs(logs, "WARN"), "a failure that persists is warned about once")
			assert.Equal(t, 1, countFailureLogs(logs, "DEBUG"))

			// The engine answers again, then fails: that's a new run.
			engine.setErr(nil)
			lookUp()
			engine.setErr(secrets.ErrAccessDenied)
			lookUp()
			assert.Equal(t, 2, countFailureLogs(logs, "WARN"))
		})
	})
}

// captureLogs records everything logged for the rest of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

// countFailureLogs counts the engine failures logged at level.
func countFailureLogs(logs *bytes.Buffer, level string) int {
	n := 0
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "level="+level) && strings.Contains(line, `msg="`+secretsEngineFailureMsg+`"`) {
			n++
		}
	}
	return n
}
