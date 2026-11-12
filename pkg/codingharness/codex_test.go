package codingharness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/harness"
)

func TestCodexLargeJSONEvents(t *testing.T) {
	for _, size := range []int{10000, 64 * 1024, 1024 * 1024} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			text := strings.Repeat("界", size)
			item, err := json.Marshal(map[string]any{
				"type": "item.completed",
				"item": map[string]any{"type": "agent_message", "text": text},
			})
			require.NoError(t, err)
			useCodexCLI(t, string(item)+"\n"+codexCompleted, 0)
			p, err := Factory(&latest.HarnessConfig{Type: TypeCodex})
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			var received strings.Builder
			err = p.Run(ctx, "test", func(ev harness.Event) {
				if ev.Type == harness.EventText {
					received.WriteString(ev.Text)
				}
			})
			require.NoError(t, err)
			assert.Equal(t, text, received.String())
		})
	}
}

const codexCompleted = `{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}` + "\n"

func TestCodexTurnOutcomes(t *testing.T) {
	for _, tt := range []struct {
		name   string
		output string
		exit   int
		want   string
	}{
		{name: "success", output: codexCompleted},
		{name: "reconnected", output: `{"type":"error","message":"Reconnecting... 1/5"}` + "\n" + codexCompleted},
		{name: "unknown event", output: `{"type":"future.event","message":{"detail":"ok"},"error":[1,2]}` + "\n" + codexCompleted},
		{name: "whitespace and unterminated line", output: "\r\n \t" + strings.TrimSpace(codexCompleted)},
		{name: "failed with zero exit", output: `{"type":"turn.failed","error":{"message":"quota exceeded"}}` + "\n", want: "codex turn failed"},
		{name: "failed with nonzero exit", output: `{"type":"turn.failed","error":{"message":"quota exceeded"}}` + "\n", exit: 1, want: "codex turn failed"},
		{name: "error then EOF", output: `{"type":"error","message":"connection lost"}` + "\n", want: "without turn.completed"},
		{name: "missing terminal event", output: `{"type":"turn.started"}` + "\n", want: "without turn.completed"},
		{name: "empty output", want: "without turn.completed"},
		{name: "invalid JSON", output: "{broken\n" + codexCompleted, want: "invalid Codex JSON event"},
		{name: "invalid envelope", output: "[]\n" + codexCompleted, want: "invalid Codex JSON event"},
		{name: "missing type", output: "{}\n" + codexCompleted, want: "missing event type"},
		{name: "nonzero exit after completion", output: codexCompleted, exit: 2, want: "exit status 2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useCodexCLI(t, tt.output, tt.exit)
			p, err := Factory(&latest.HarnessConfig{Type: TypeCodex})
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			err = p.Run(ctx, "test", func(harness.Event) {})
			if tt.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.want)
			}
			assert.NoError(t, ctx.Err(), "runner must finish without the watchdog")
		})
	}
}

func TestCodexResumeArguments(t *testing.T) {
	argsPath := useCodexCLI(t, codexCompleted, 0)
	p, err := Factory(&latest.HarnessConfig{Type: TypeCodex, Model: "gpt-model"})
	require.NoError(t, err)
	require.NoError(t, p.Resume(t.Context(), "thread-id", "don't interpolate $(echo unsafe)", func(harness.Event) {}))
	args, err := os.ReadFile(argsPath)
	require.NoError(t, err)
	assert.Equal(t, "exec\nresume\nthread-id\n--json\n--dangerously-bypass-approvals-and-sandbox\n-m\ngpt-model\ndon't interpolate $(echo unsafe)\n", string(args))
	require.ErrorContains(t, p.Resume(t.Context(), "", "test", func(harness.Event) {}), "session ID is empty")
}

func TestCodexOversizedEventReturnsWithoutHanging(t *testing.T) {
	useCodexCLI(t, `{"type":"item.completed","item":{"type":"agent_message","text":"`+strings.Repeat("x", 17*1024*1024)+`"}}`+"\n", 0)
	p, err := Factory(&latest.HarnessConfig{Type: TypeCodex})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err = p.Run(ctx, "test", func(harness.Event) {})
	require.ErrorContains(t, err, "stream line exceeds")
	assert.NoError(t, ctx.Err(), "size rejection must not wait for the subprocess")
}

func useCodexCLI(t *testing.T, output string, exit int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script shim test")
	}
	dir := t.TempDir()
	outPath := filepath.Join(dir, "events.jsonl")
	argsPath := filepath.Join(dir, "args")
	require.NoError(t, os.WriteFile(outPath, []byte(output), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "exit-code"), []byte(strconv.Itoa(exit)+"\n"), 0o600))
	// Reuse the executable inode to avoid macOS's per-file cold-exec checks.
	binary, err := filepath.Abs("testdata/codex")
	require.NoError(t, err)
	require.NoError(t, os.Symlink(binary, filepath.Join(dir, "codex")))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsPath
}

func TestCodexCancellation(t *testing.T) {
	useCodexCLI(t, "", 0)
	dir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	require.NoError(t, os.WriteFile(filepath.Join(dir, "wait"), nil, 0o600))
	p, err := Factory(&latest.HarnessConfig{Type: TypeCodex})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watchdog, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	err = p.Run(watchdog, "test", func(ev harness.Event) {
		if ev.Type == harness.EventSessionID {
			cancel()
		}
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestCodexSlowConsumer(t *testing.T) {
	useCodexCLI(t, `{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}`+"\n"+codexCompleted, 0)
	p, err := Factory(&latest.HarnessConfig{Type: TypeCodex})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err = p.Run(ctx, "test", func(ev harness.Event) {
		if ev.Type == harness.EventText {
			// The leader exits while the consumer is still handling this event.
			<-time.After(2200 * time.Millisecond)
		}
	})
	require.NoError(t, err)
}

func TestCodexLargeToolOutput(t *testing.T) {
	output := strings.Repeat("output\n", 20000)
	item, err := json.Marshal(map[string]any{
		"type": "item.completed",
		"item": map[string]any{"type": "command_execution", "id": "tool-1", "aggregated_output": output, "exit_code": 0},
	})
	require.NoError(t, err)
	useCodexCLI(t, string(item)+"\n"+codexCompleted, 0)
	p, err := Factory(&latest.HarnessConfig{Type: TypeCodex})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var result harness.Event
	require.NoError(t, p.Run(ctx, "test", func(ev harness.Event) {
		if ev.Type == harness.EventToolResult {
			result = ev
		}
	}))
	assert.Equal(t, "tool-1", result.ToolID)
	assert.Equal(t, output, result.ToolOutput)
	assert.False(t, result.ToolError)
}

func TestCodexValidationPerInvocation(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(strconv.FormatBool(resume), func(t *testing.T) {
			useCodexCLI(t, codexCompleted, 0)
			p, err := Factory(&latest.HarnessConfig{Type: TypeCodex})
			require.NoError(t, err)
			run := func(ctx context.Context) error {
				if resume {
					return p.Resume(ctx, "thread-id", "test", func(harness.Event) {})
				}
				return p.Run(ctx, "test", func(harness.Event) {})
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			require.NoError(t, run(ctx))
			dir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
			require.NoError(t, os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(`{"type":"turn.started"}`+"\n"), 0o600))
			require.ErrorContains(t, run(ctx), "without turn.completed")
		})
	}
}

func TestCodexErrorsOmitStreamContents(t *testing.T) {
	for _, output := range []string{
		`{"type":"turn.failed","error":{"message":"private-output-marker"}}` + "\n",
		`{"type":"error","message":"private-output-marker"}` + "\n",
		`{"type":"private-output-marker` + "\n",
	} {
		useCodexCLI(t, output, 0)
		p, err := Factory(&latest.HarnessConfig{Type: TypeCodex})
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		err = p.Run(ctx, "test", func(harness.Event) {})
		cancel()
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "private-output-marker")
	}
}
