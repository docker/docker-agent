package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

// fakeLSPServerEnv names the environment variable that tells this test
// binary, when re-executed as a subprocess, to behave as a fake LSP server
// instead of running the Go test suite. Its value is the path of a log
// file the fake server appends one line to on every spawn, so tests can
// count how many times the supervisor actually spawned a new process.
const fakeLSPServerEnv = "DOCKER_AGENT_LSP_TEST_FAKE_SERVER_LOG"

// TestMain lets this test binary re-exec itself (os.Args[0]) as a fake LSP
// server: exec.Command needs a real, portable executable, and the test
// binary itself is the simplest one available on every platform CI runs on.
func TestMain(m *testing.M) {
	if logPath := os.Getenv(fakeLSPServerEnv); logPath != "" {
		runFakeCrashingLSPServer(logPath)
		return // unreachable: runFakeCrashingLSPServer always calls os.Exit.
	}
	os.Exit(m.Run())
}

// runFakeCrashingLSPServer answers the initialize/initialized handshake
// exactly once, records that it ran, then exits non-zero to simulate a
// crash right after startup — every time it is spawned.
func runFakeCrashingLSPServer(logPath string) {
	if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintln(f, "spawn")
		_ = f.Close()
	}

	r := bufio.NewReader(os.Stdin)
	if body, err := readFramedMessage(r); err == nil {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(body, &req) == nil && req.Method == "initialize" {
			resp, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{"capabilities": map[string]any{}},
			})
			if writeFramedMessage(os.Stdout, resp) == nil {
				_, _ = readFramedMessage(r) // the "initialized" notification; ignored.
			}
		}
	}
	os.Exit(1)
}

// readFramedMessage and writeFramedMessage mirror the Content-Length
// framing lspHandler itself speaks (see readMessageLocked/writeMessageLocked).
func readFramedMessage(r *bufio.Reader) ([]byte, error) {
	contentLength := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if after, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			contentLength, err = strconv.Atoi(strings.TrimSpace(after))
			if err != nil {
				return nil, err
			}
		}
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func writeFramedMessage(w io.Writer, data []byte) error {
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(data)); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// countSpawns counts lines in the fake server's spawn log, i.e. how many
// times it has actually been launched as a subprocess.
func countSpawns(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("failed to read spawn log: %v", err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

// TestLSPTool_CrashLoopArmsBackoffGate drives a real ToolSet, wrapped in
// tools.StartableToolSet exactly as production wires it, against a fake
// LSP server that completes the handshake and then exits non-zero every
// time it is spawned. It proves the whole chain end to end: a sustained
// crash loop stops the supervisor's own auto-restart, TryStart surfaces
// lifecycle.ErrCrashLooping, and the backoff gate then withholds further
// spawns until its window elapses.
func TestLSPTool_CrashLoopArmsBackoffGate(t *testing.T) {
	t.Parallel()

	spawnLog := filepath.Join(t.TempDir(), "spawns.log")
	env := []string{fakeLSPServerEnv + "=" + spawnLog}

	policy := lifecycle.Policy{
		Backoff:   lifecycle.Backoff{Initial: 2 * time.Millisecond, Max: 5 * time.Millisecond, Multiplier: 2},
		CrashLoop: lifecycle.CrashLoop{Threshold: 3, Window: time.Minute},
	}
	tool := New(os.Args[0], nil, env, t.TempDir(), policy)
	s := tools.NewStartable(tool, tools.WithStartRetryJitter(func(d time.Duration) time.Duration { return d }))
	t.Cleanup(func() { _ = s.Stop(t.Context()) })

	started, err := s.TryStart(t.Context())
	require.NoError(t, err)
	require.True(t, started)

	// The fake server crashes right after the handshake every time; wait
	// for the crash-loop detector to give up (state -> Failed) rather than
	// racing the background watcher's own restart attempts.
	require.Eventually(t, func() bool {
		return tool.State().State == lifecycle.StateFailed
	}, 10*time.Second, 5*time.Millisecond, "supervisor did not detect the crash loop")

	spawnsAtLoop := countSpawns(t, spawnLog)
	assert.Equal(t, 3, spawnsAtLoop, "the loop must trip at exactly CrashLoop.Threshold spawns, no more")

	// The next TryStart reports the loop instead of relaunching.
	_, err = s.TryStart(t.Context())
	require.ErrorIs(t, err, lifecycle.ErrCrashLooping)
	assert.Equal(t, spawnsAtLoop, countSpawns(t, spawnLog), "the crash-loop report must not itself spawn a server")

	// Gate now armed: an immediate retry must not spawn either.
	_, err = s.TryStart(t.Context())
	require.Error(t, err)
	assert.Equal(t, spawnsAtLoop, countSpawns(t, spawnLog), "gate must withhold the next spawn until its window elapses")
}

// TestLSPTool_CrashLoopPendingReportBlocksDirectToolCalls verifies that a
// tool call reaching the LSP handler while a crash-loop report is pending
// — ensureInitialized's lazy per-request path, which bypasses
// tools.StartableToolSet entirely — fails fast on the same error without
// spawning a new server, and without consuming the one-shot report: only
// the wrapper's own paced retry (Start, once its backoff window elapses)
// may do that.
func TestLSPTool_CrashLoopPendingReportBlocksDirectToolCalls(t *testing.T) {
	t.Parallel()

	spawnLog := filepath.Join(t.TempDir(), "spawns.log")
	env := []string{fakeLSPServerEnv + "=" + spawnLog}

	policy := lifecycle.Policy{
		Backoff:   lifecycle.Backoff{Initial: 2 * time.Millisecond, Max: 5 * time.Millisecond, Multiplier: 2},
		CrashLoop: lifecycle.CrashLoop{Threshold: 3, Window: time.Minute},
	}
	tool := New(os.Args[0], nil, env, t.TempDir(), policy)
	t.Cleanup(func() { _ = tool.Stop(t.Context()) })

	require.NoError(t, tool.Start(t.Context()))

	require.Eventually(t, func() bool {
		return tool.State().State == lifecycle.StateFailed
	}, 10*time.Second, 5*time.Millisecond, "supervisor did not detect the crash loop")

	spawnsAtLoop := countSpawns(t, spawnLog)
	assert.Equal(t, 3, spawnsAtLoop)

	// ensureInitialized has a fast path keyed on the atomic `initialized`
	// flag, which a raw crash (detected only in the background watcher)
	// does not clear — only a fresh Connect or an explicit Close does. That
	// pre-existing gap (independent of crash-loop pacing; it also affects
	// the ordinary exhausted-restart give-up) means a tool call arriving
	// immediately after this crash would still see the stale flag and skip
	// the check below entirely. Clear it here to exercise the check as it
	// would run once that flag correctly reflects the disconnect.
	tool.handler.initialized.Store(false)

	// A tool call arriving now goes through ensureInitialized, not
	// StartableToolSet.TryStart. It must see the same error and must not
	// spawn a server on its own.
	err := tool.handler.ensureInitialized(t.Context())
	require.ErrorIs(t, err, lifecycle.ErrCrashLooping)
	assert.Equal(t, spawnsAtLoop, countSpawns(t, spawnLog), "a direct tool call must not spawn while a crash-loop report is pending")

	// The report must still be pending for the next caller: a direct call
	// must not have consumed it.
	require.ErrorIs(t, tool.handler.supervisor.PendingCrashLoopError(), lifecycle.ErrCrashLooping)

	// Only the "real" gated caller (standing in for StartableToolSet's
	// paced Restart/Start once its window elapses) may consume it and
	// reconnect for real: the very next Start still reports it once
	// (ensureInitialized's peek above did not consume it), and the Start
	// after that performs the genuine reconnect.
	err = tool.Start(t.Context())
	require.ErrorIs(t, err, lifecycle.ErrCrashLooping)
	assert.Equal(t, spawnsAtLoop, countSpawns(t, spawnLog), "the consuming Start must not itself spawn either")

	require.NoError(t, tool.Start(t.Context()))
	assert.Equal(t, spawnsAtLoop+1, countSpawns(t, spawnLog))
	assert.NoError(t, tool.handler.supervisor.PendingCrashLoopError())
}
