package shell

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/tools"
)

func acceptHandler(password string) func() tools.ElicitationHandler {
	return func() tools.ElicitationHandler {
		return func(_ context.Context, _ *mcp.ElicitParams) (tools.ElicitationResult, error) {
			return tools.ElicitationResult{
				Action:  tools.ElicitationActionAccept,
				Content: map[string]any{"password": password},
			}, nil
		}
	}
}

func TestAskpass_RoundTrip(t *testing.T) {
	if !askpassSupported() {
		t.Skip("sudo askpass unsupported on this platform")
	}

	srv, err := startAskpassServer(acceptHandler("hunter2"))
	require.NoError(t, err)
	defer srv.close()

	t.Setenv(envAskpassSocket, srv.socket)
	t.Setenv(envAskpassToken, srv.token)

	var out bytes.Buffer
	require.NoError(t, RunAskpassClient(t.Context(), "[sudo] password for user:", &out))
	assert.Equal(t, "hunter2\n", out.String())
}

func TestAskpass_Decline(t *testing.T) {
	if !askpassSupported() {
		t.Skip("sudo askpass unsupported on this platform")
	}

	srv, err := startAskpassServer(func() tools.ElicitationHandler {
		return func(_ context.Context, _ *mcp.ElicitParams) (tools.ElicitationResult, error) {
			return tools.ElicitationResult{Action: tools.ElicitationActionDecline}, nil
		}
	})
	require.NoError(t, err)
	defer srv.close()

	t.Setenv(envAskpassSocket, srv.socket)
	t.Setenv(envAskpassToken, srv.token)

	var out bytes.Buffer
	require.Error(t, RunAskpassClient(t.Context(), "prompt", &out))
	assert.Empty(t, out.String())
}

func TestAskpass_BadToken(t *testing.T) {
	if !askpassSupported() {
		t.Skip("sudo askpass unsupported on this platform")
	}

	srv, err := startAskpassServer(acceptHandler("hunter2"))
	require.NoError(t, err)
	defer srv.close()

	t.Setenv(envAskpassSocket, srv.socket)
	t.Setenv(envAskpassToken, "wrong-token")

	var out bytes.Buffer
	require.Error(t, RunAskpassClient(t.Context(), "prompt", &out))
	assert.Empty(t, out.String())
}

func TestAskpass_MissingEnv(t *testing.T) {
	t.Setenv(envAskpassSocket, "")
	t.Setenv(envAskpassToken, "")

	var out bytes.Buffer
	require.Error(t, RunAskpassClient(t.Context(), "prompt", &out))
}

func TestShQuote(t *testing.T) {
	// No metacharacters: simple single-quote wrap.
	assert.Equal(t, "'/usr/bin/docker-agent'", shQuote("/usr/bin/docker-agent"))
	// Embedded single quote is escaped.
	assert.Equal(t, `'a'\''b'`, shQuote("a'b"))
	// Command-substitution metacharacters are inert inside single quotes.
	assert.Equal(t, `'/tmp/x$(touch pwned)/a'`, shQuote("/tmp/x$(touch pwned)/a"))
}

// TestAskpass_CancelledWhenHelperDies verifies the prompt is cancelled when the
// dialing helper goes away (its command was killed), instead of blocking on a
// dead connection for the full prompt timeout.
func TestAskpass_CancelledWhenHelperDies(t *testing.T) {
	if !askpassSupported() {
		t.Skip("sudo askpass unsupported on this platform")
	}

	cancelled := make(chan struct{})
	srv, err := startAskpassServer(func() tools.ElicitationHandler {
		return func(ctx context.Context, _ *mcp.ElicitParams) (tools.ElicitationResult, error) {
			<-ctx.Done() // block like a real prompt until the context is cancelled
			close(cancelled)
			return tools.ElicitationResult{}, ctx.Err()
		}
	})
	require.NoError(t, err)
	defer srv.close()

	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "unix", srv.socket)
	require.NoError(t, err)
	req, _ := json.Marshal(askpassRequest{Token: srv.token, Prompt: "p"})
	_, err = conn.Write(append(req, '\n'))
	require.NoError(t, err)

	// Simulate the helper dying (killed with its command's process group).
	require.NoError(t, conn.Close())

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt was not cancelled after the helper connection closed")
	}
}

func TestCommandInvokesSudo(t *testing.T) {
	assert.True(t, commandInvokesSudo("sudo apt update"))
	assert.True(t, commandInvokesSudo("echo x && sudo ls"))
	// Word boundary avoids false positives on similar substrings.
	assert.False(t, commandInvokesSudo("echo pseudo"))
	assert.False(t, commandInvokesSudo("ls /etc/sudoers"))
	assert.False(t, commandInvokesSudo("play sudoku"))
	assert.False(t, commandInvokesSudo("ls -la"))
}

// TestAskpass_PromptsSerialized verifies two concurrent sudo prompts never open
// two dialogs at once (the runtime carries a single elicitation request).
func TestAskpass_PromptsSerialized(t *testing.T) {
	if !askpassSupported() {
		t.Skip("sudo askpass unsupported on this platform")
	}

	var active, maxActive int32
	srv, err := startAskpassServer(func() tools.ElicitationHandler {
		return func(_ context.Context, _ *mcp.ElicitParams) (tools.ElicitationResult, error) {
			n := atomic.AddInt32(&active, 1)
			for {
				m := atomic.LoadInt32(&maxActive)
				if n <= m || atomic.CompareAndSwapInt32(&maxActive, m, n) {
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
			atomic.AddInt32(&active, -1)
			return tools.ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"password": "pw"}}, nil
		}
	})
	require.NoError(t, err)
	defer srv.close()

	t.Setenv(envAskpassSocket, srv.socket)
	t.Setenv(envAskpassToken, srv.token)

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			var out bytes.Buffer
			assert.NoError(t, RunAskpassClient(t.Context(), "p", &out))
		})
	}
	wg.Wait()

	assert.Equal(t, int32(1), atomic.LoadInt32(&maxActive), "prompts must be serialized to one at a time")
}

func TestWrapSudoCommand(t *testing.T) {
	t.Run("wraps sudo for posix shell", func(t *testing.T) {
		got := wrapSudoCommand("sudo apt update", "/bin/bash")
		assert.Contains(t, got, `sudo() { command sudo -A "$@"; }`)
		assert.Contains(t, got, "sudo apt update")
	})

	t.Run("leaves non-sudo commands untouched", func(t *testing.T) {
		assert.Equal(t, "ls -la", wrapSudoCommand("ls -la", "/bin/bash"))
	})

	t.Run("leaves non-posix shells untouched", func(t *testing.T) {
		assert.Equal(t, "sudo apt update", wrapSudoCommand("sudo apt update", "/usr/bin/fish"))
	})
}

// TestShellToolSet_ElicitableThroughWrappers guards the wiring the askpass flow
// depends on: the runtime applies the elicitation handler via
// tools.ConfigureHandlers, which finds the capability with tools.As. The shell
// toolset is wrapped by NewStartable and WithName before that runs, so As must
// be able to reach the inner ToolSet through both wrappers.
func TestShellToolSet_ElicitableThroughWrappers(t *testing.T) {
	ts := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	wrapped := tools.WithName(tools.NewStartable(ts), "shell")

	e, ok := tools.As[tools.Elicitable](wrapped)
	require.True(t, ok, "shell toolset must be discoverable as Elicitable through its wrappers")

	e.SetElicitationHandler(func(_ context.Context, _ *mcp.ElicitParams) (tools.ElicitationResult, error) {
		return tools.ElicitationResult{}, nil
	})
	assert.NotNil(t, ts.handler.currentElicitationHandler(), "handler must reach the inner shell handler")
}

func TestAskpassActive(t *testing.T) {
	h := &shellHandler{sudoAskpass: true}
	// No elicitation handler yet -> inactive.
	assert.False(t, h.askpassActive())

	h.setElicitationHandler(func(_ context.Context, _ *mcp.ElicitParams) (tools.ElicitationResult, error) {
		return tools.ElicitationResult{Action: tools.ElicitationActionDecline}, nil
	})
	assert.Equal(t, askpassSupported(), h.askpassActive())

	// Disabled in config -> never active even with a handler.
	h.sudoAskpass = false
	assert.False(t, h.askpassActive())
}
