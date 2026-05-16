package hooks

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHookGetTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		hook     Hook
		expected time.Duration
	}{
		{
			name:     "default timeout",
			hook:     Hook{},
			expected: 60 * time.Second,
		},
		{
			name:     "zero timeout uses default",
			hook:     Hook{Timeout: 0},
			expected: 60 * time.Second,
		},
		{
			name:     "negative timeout uses default",
			hook:     Hook{Timeout: -1},
			expected: 60 * time.Second,
		},
		{
			name:     "custom timeout",
			hook:     Hook{Timeout: 30},
			expected: 30 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, tt.hook.GetTimeout())
		})
	}
}

func TestConfigIsEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		config   Config
		expected bool
	}{
		{
			name:     "empty config",
			config:   Config{},
			expected: true,
		},
		{
			name: "with pre_tool_use",
			config: Config{
				PreToolUse: []MatcherConfig{{Matcher: "*", Hooks: []Hook{}}},
			},
			expected: false,
		},
		{
			name: "with post_tool_use",
			config: Config{
				PostToolUse: []MatcherConfig{{Matcher: "*"}},
			},
			expected: false,
		},
		{
			name: "with session_start",
			config: Config{
				SessionStart: []Hook{{Type: HookTypeCommand}},
			},
			expected: false,
		},
		{
			name: "with session_end",
			config: Config{
				SessionEnd: []Hook{{Type: HookTypeCommand}},
			},
			expected: false,
		},
		{
			name: "with on_user_input",
			config: Config{
				OnUserInput: []Hook{{Type: HookTypeCommand}},
			},
			expected: false,
		},
		{
			name: "with stop",
			config: Config{
				Stop: []Hook{{Type: HookTypeCommand}},
			},
			expected: false,
		},
		{
			name: "with notification",
			config: Config{
				Notification: []Hook{{Type: HookTypeCommand}},
			},
			expected: false,
		},
		{
			name: "with before_compaction",
			config: Config{
				BeforeCompaction: []Hook{{Type: HookTypeCommand}},
			},
			expected: false,
		},
		{
			name: "with after_compaction",
			config: Config{
				AfterCompaction: []Hook{{Type: HookTypeCommand}},
			},
			expected: false,
		},
		{
			name: "with turn_end",
			config: Config{
				TurnEnd: []Hook{{Type: HookTypeCommand}},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, tt.config.IsEmpty())
		})
	}
}

func TestInputToJSON(t *testing.T) {
	t.Parallel()

	input := &Input{
		SessionID:     "sess-123",
		Cwd:           "/tmp",
		HookEventName: EventPreToolUse,
		ToolName:      "shell",
		ToolUseID:     "tool-456",
		ToolInput: map[string]any{
			"cmd": "ls -la",
			"cwd": ".",
		},
	}

	data, err := input.ToJSON()
	require.NoError(t, err)

	var parsed map[string]any
	err = json.Unmarshal(data, &parsed)
	require.NoError(t, err)

	assert.Equal(t, "sess-123", parsed["session_id"])
	assert.Equal(t, "/tmp", parsed["cwd"])
	assert.Equal(t, "pre_tool_use", parsed["hook_event_name"])
	assert.Equal(t, "shell", parsed["tool_name"])
	assert.Equal(t, "tool-456", parsed["tool_use_id"])
}

func TestOutputShouldContinue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		output   Output
		expected bool
	}{
		{
			name:     "nil continue defaults to true",
			output:   Output{},
			expected: true,
		},
		{
			name:     "continue true",
			output:   Output{Continue: new(true)},
			expected: true,
		},
		{
			name:     "continue false",
			output:   Output{Continue: new(false)},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, tt.output.ShouldContinue())
		})
	}
}

func TestOutputIsBlocked(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		output   Output
		expected bool
	}{
		{
			name:     "empty decision",
			output:   Output{},
			expected: false,
		},
		{
			name:     "block decision",
			output:   Output{Decision: "block"},
			expected: true,
		},
		{
			name:     "allow decision",
			output:   Output{Decision: "allow"},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, tt.output.IsBlocked())
		})
	}
}

func TestNewExecutor(t *testing.T) {
	t.Parallel()

	config := &Config{
		PreToolUse: []MatcherConfig{
			{
				Matcher: "shell|edit_file",
				Hooks: []Hook{
					{Type: HookTypeCommand, Command: "echo pre"},
				},
			},
		},
	}

	exec := NewExecutor(config, "/tmp", []string{"FOO=bar"})
	require.NotNil(t, exec)
	assert.True(t, exec.Has(EventPreToolUse))
	assert.False(t, exec.Has(EventPostToolUse))
	assert.False(t, exec.Has(EventSessionStart))
	assert.False(t, exec.Has(EventSessionEnd))
	assert.False(t, exec.Has(EventStop))
	assert.False(t, exec.Has(EventNotification))
}

func TestExecutorNilConfig(t *testing.T) {
	t.Parallel()

	exec := NewExecutor(nil, "/tmp", nil)
	require.NotNil(t, exec)
	assert.False(t, exec.Has(EventPreToolUse))
}

func TestCompiledMatcherMatchTool(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		matcher  string
		toolName string
		expected bool
	}{
		{
			name:     "wildcard matches any",
			matcher:  "*",
			toolName: "shell",
			expected: true,
		},
		{
			name:     "empty matcher matches any",
			matcher:  "",
			toolName: "shell",
			expected: true,
		},
		{
			name:     "exact match",
			matcher:  "shell",
			toolName: "shell",
			expected: true,
		},
		{
			name:     "exact match fails",
			matcher:  "shell",
			toolName: "edit_file",
			expected: false,
		},
		{
			name:     "alternation match first",
			matcher:  "shell|edit_file",
			toolName: "shell",
			expected: true,
		},
		{
			name:     "alternation match second",
			matcher:  "shell|edit_file",
			toolName: "edit_file",
			expected: true,
		},
		{
			name:     "alternation no match",
			matcher:  "shell|edit_file",
			toolName: "write_file",
			expected: false,
		},
		{
			name:     "regex pattern",
			matcher:  "mcp__.*",
			toolName: "mcp__github_list_repos",
			expected: true,
		},
		{
			name:     "regex pattern no match",
			matcher:  "mcp__.*",
			toolName: "shell",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			config := &Config{
				PreToolUse: []MatcherConfig{
					{Matcher: tt.matcher, Hooks: []Hook{{Type: HookTypeCommand, Command: "echo test"}}},
				},
			}
			exec := NewExecutor(config, "/tmp", nil)
			matchers := exec.events[EventPreToolUse]
			require.Len(t, matchers, 1)
			assert.Equal(t, tt.expected, matchers[0].matches(tt.toolName))
		})
	}
}

func TestExecutePreToolUseWithEchoCommand(t *testing.T) {
	t.Parallel()

	config := &Config{
		PreToolUse: []MatcherConfig{
			{
				Matcher: "*",
				Hooks: []Hook{
					{Type: HookTypeCommand, Command: "echo 'test'", Timeout: 5},
				},
			},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID: "test-session",
		ToolName:  "shell",
		ToolUseID: "test-id",
	}

	result, err := exec.Dispatch(t.Context(), EventPreToolUse, input)
	require.NoError(t, err)
	assert.True(t, result.Allowed)
}

func TestExecutePreToolUseBlockingExitCode(t *testing.T) {
	t.Parallel()

	config := &Config{
		PreToolUse: []MatcherConfig{
			{
				Matcher: "*",
				Hooks: []Hook{
					{Type: HookTypeCommand, Command: "exit 2", Timeout: 5},
				},
			},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID: "test-session",
		ToolName:  "shell",
		ToolUseID: "test-id",
	}

	result, err := exec.Dispatch(t.Context(), EventPreToolUse, input)
	require.NoError(t, err)
	assert.False(t, result.Allowed)
	assert.Equal(t, 2, result.ExitCode)
}

func TestExecutePreToolUseNoMatchingHooks(t *testing.T) {
	t.Parallel()

	config := &Config{
		PreToolUse: []MatcherConfig{
			{
				Matcher: "edit_file",
				Hooks: []Hook{
					{Type: HookTypeCommand, Command: "exit 2", Timeout: 5},
				},
			},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID: "test-session",
		ToolName:  "shell", // Doesn't match "edit_file"
		ToolUseID: "test-id",
	}

	result, err := exec.Dispatch(t.Context(), EventPreToolUse, input)
	require.NoError(t, err)
	assert.True(t, result.Allowed) // Should be allowed since no hooks matched
}

func TestExecutePreToolUseWithJSONOutput(t *testing.T) {
	t.Parallel()

	jsonOutput := `{"decision":"block","reason":"Tool not allowed"}`
	config := &Config{
		PreToolUse: []MatcherConfig{
			{
				Matcher: "*",
				Hooks: []Hook{
					{Type: HookTypeCommand, Command: "echo '" + jsonOutput + "'", Timeout: 5},
				},
			},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID: "test-session",
		ToolName:  "shell",
		ToolUseID: "test-id",
	}

	result, err := exec.Dispatch(t.Context(), EventPreToolUse, input)
	require.NoError(t, err)
	assert.False(t, result.Allowed)
	assert.Contains(t, result.Message, "Tool not allowed")
}

func TestExecutePostToolUse(t *testing.T) {
	t.Parallel()

	config := &Config{
		PostToolUse: []MatcherConfig{
			{
				Matcher: "shell",
				Hooks: []Hook{
					{Type: HookTypeCommand, Command: "echo 'post-hook'", Timeout: 5},
				},
			},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID:    "test-session",
		ToolName:     "shell",
		ToolUseID:    "test-id",
		ToolResponse: "command output",
	}

	result, err := exec.Dispatch(t.Context(), EventPostToolUse, input)
	require.NoError(t, err)
	assert.True(t, result.Allowed)
}

func TestExecuteSessionStart(t *testing.T) {
	t.Parallel()

	config := &Config{
		SessionStart: []Hook{
			{Type: HookTypeCommand, Command: "echo 'session starting'", Timeout: 5},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID: "test-session",
		Source:    "startup",
	}

	result, err := exec.Dispatch(t.Context(), EventSessionStart, input)
	require.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Contains(t, result.AdditionalContext, "session starting")
}

func TestExecuteSessionEnd(t *testing.T) {
	t.Parallel()

	config := &Config{
		SessionEnd: []Hook{
			{Type: HookTypeCommand, Command: "echo 'session ending'", Timeout: 5},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID: "test-session",
		Reason:    "logout",
	}

	result, err := exec.Dispatch(t.Context(), EventSessionEnd, input)
	require.NoError(t, err)
	assert.True(t, result.Allowed)
}

func TestExecuteOnUserInput(t *testing.T) {
	t.Parallel()

	config := &Config{
		OnUserInput: []Hook{
			{Type: HookTypeCommand, Command: "echo 'user input needed'", Timeout: 5},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID: "test-session",
	}

	result, err := exec.Dispatch(t.Context(), EventOnUserInput, input)
	require.NoError(t, err)
	assert.True(t, result.Allowed)
}

func TestExecuteStop(t *testing.T) {
	t.Parallel()

	config := &Config{
		Stop: []Hook{
			{Type: HookTypeCommand, Command: "echo 'model stopped'", Timeout: 5},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID:    "test-session",
		StopResponse: "Here is the answer to your question.",
	}

	result, err := exec.Dispatch(t.Context(), EventStop, input)
	require.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Contains(t, result.AdditionalContext, "model stopped")
}

func TestExecuteStopReceivesResponseContent(t *testing.T) {
	t.Parallel()

	config := &Config{
		Stop: []Hook{
			{Type: HookTypeCommand, Command: "cat | jq -r '.stop_response'", Timeout: 5},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID:    "test-session",
		StopResponse: "final answer content",
	}

	result, err := exec.Dispatch(t.Context(), EventStop, input)
	require.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Contains(t, result.AdditionalContext, "final answer content")
}

func TestExecuteNotification(t *testing.T) {
	t.Parallel()

	config := &Config{
		Notification: []Hook{
			{Type: HookTypeCommand, Command: "echo 'notification received'", Timeout: 5},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID:           "test-session",
		NotificationLevel:   "error",
		NotificationMessage: "Something went wrong",
	}

	result, err := exec.Dispatch(t.Context(), EventNotification, input)
	require.NoError(t, err)
	assert.True(t, result.Allowed)
}

func TestExecuteNotificationReceivesLevel(t *testing.T) {
	t.Parallel()

	config := &Config{
		Notification: []Hook{
			{Type: HookTypeCommand, Command: "cat | jq -r '.notification_level'", Timeout: 5},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID:           "test-session",
		NotificationLevel:   "warning",
		NotificationMessage: "Watch out",
	}

	result, err := exec.Dispatch(t.Context(), EventNotification, input)
	require.NoError(t, err)
	assert.True(t, result.Allowed)
}

func TestExecuteHooksWithContextCancellation(t *testing.T) {
	t.Parallel()

	config := &Config{
		PreToolUse: []MatcherConfig{
			{
				Matcher: "*",
				Hooks: []Hook{
					{Type: HookTypeCommand, Command: "sleep 10", Timeout: 30},
				},
			},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID: "test-session",
		ToolName:  "shell",
		ToolUseID: "test-id",
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	result, err := exec.Dispatch(ctx, EventPreToolUse, input)
	require.NoError(t, err)
	// PreToolUse is a security boundary: when the hook fails to run to
	// completion (here, the parent context was cancelled before the hook
	// could report a verdict), the tool call must be denied rather than
	// silently allowed.
	assert.False(t, result.Allowed)
	assert.Equal(t, -1, result.ExitCode)
	assert.Contains(t, result.Message, "PreToolUse hook failed to execute")
}

// A hook that exits with a non-zero, non-2 code is a non-blocking error:
// it is reported as such in the result but does not deny the tool call.
// Pair this with TestExecuteHooksWithContextCancellation, which asserts the
// opposite for execution failures (timeout, spawn error).
func TestExecutePreToolUseAllowsNonBlockingExitCode(t *testing.T) {
	t.Parallel()

	config := &Config{
		PreToolUse: []MatcherConfig{
			{
				Matcher: "*",
				Hooks: []Hook{
					{Type: HookTypeCommand, Command: "exit 1", Timeout: 5},
				},
			},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID: "test-session",
		ToolName:  "shell",
		ToolUseID: "test-id",
	}

	result, err := exec.Dispatch(t.Context(), EventPreToolUse, input)
	require.NoError(t, err)
	assert.True(t, result.Allowed)
}

// TestPlainStdoutBecomesAdditionalContext pins the contract that a
// command hook on a context-injection event (session_start, turn_start,
// post_tool_use, stop) can write plain text to stdout and have it
// appear as Result.AdditionalContext, without having to wrap in JSON.
//
// Observational events (before/after LLM call, on_error, ...) MUST
// drop plain stdout: their runtime emit sites do not surface
// AdditionalContext, so silently routing it there would let hook
// authors think their output mattered when it would actually be
// thrown away. Those hooks should structure their output as JSON if
// they want to communicate with the executor.
func TestPlainStdoutBecomesAdditionalContext(t *testing.T) {
	t.Parallel()

	contextEvents := []EventType{
		EventSessionStart, EventTurnStart, EventPostToolUse, EventStop,
	}
	observationalEvents := []EventType{
		EventBeforeLLMCall, EventAfterLLMCall, EventOnError,
		EventOnMaxIterations, EventNotification, EventOnUserInput, EventSessionEnd,
		EventBeforeCompaction, EventAfterCompaction, EventTurnEnd,
	}

	for _, ev := range contextEvents {
		t.Run(string(ev), func(t *testing.T) {
			t.Parallel()
			cfg := configWithFlatHook(ev, Hook{Type: HookTypeCommand, Command: "echo plain-text-context", Timeout: 5})
			exec := NewExecutor(cfg, t.TempDir(), nil)
			res, err := exec.Dispatch(t.Context(), ev, &Input{SessionID: "s", ToolName: "shell"})
			require.NoError(t, err)
			assert.True(t, res.Allowed)
			assert.Contains(t, res.AdditionalContext, "plain-text-context",
				"%s should route plain stdout into AdditionalContext", ev)
		})
	}

	for _, ev := range observationalEvents {
		t.Run(string(ev), func(t *testing.T) {
			t.Parallel()
			cfg := configWithFlatHook(ev, Hook{Type: HookTypeCommand, Command: "echo would-be-dropped", Timeout: 5})
			exec := NewExecutor(cfg, t.TempDir(), nil)
			res, err := exec.Dispatch(t.Context(), ev, &Input{SessionID: "s", ToolName: "shell"})
			require.NoError(t, err)
			assert.True(t, res.Allowed)
			assert.Empty(t, res.AdditionalContext,
				"%s must NOT surface plain stdout as AdditionalContext (the runtime’s emit site doesn’t consume it)", ev)
		})
	}
}

// configWithFlatHook builds a Config that wires the given hook into the
// flat slice for ev. Tool-matched events go through their MatcherConfig
// shape; everything else uses the bare []Hook field.
func configWithFlatHook(ev EventType, h Hook) *Config {
	cfg := &Config{}
	switch ev {
	case EventPreToolUse:
		cfg.PreToolUse = []MatcherConfig{{Matcher: "*", Hooks: []Hook{h}}}
	case EventPostToolUse:
		cfg.PostToolUse = []MatcherConfig{{Matcher: "*", Hooks: []Hook{h}}}
	case EventSessionStart:
		cfg.SessionStart = []Hook{h}
	case EventTurnStart:
		cfg.TurnStart = []Hook{h}
	case EventTurnEnd:
		cfg.TurnEnd = []Hook{h}
	case EventBeforeLLMCall:
		cfg.BeforeLLMCall = []Hook{h}
	case EventAfterLLMCall:
		cfg.AfterLLMCall = []Hook{h}
	case EventSessionEnd:
		cfg.SessionEnd = []Hook{h}
	case EventOnUserInput:
		cfg.OnUserInput = []Hook{h}
	case EventStop:
		cfg.Stop = []Hook{h}
	case EventNotification:
		cfg.Notification = []Hook{h}
	case EventOnError:
		cfg.OnError = []Hook{h}
	case EventOnMaxIterations:
		cfg.OnMaxIterations = []Hook{h}
	case EventBeforeCompaction:
		cfg.BeforeCompaction = []Hook{h}
	case EventAfterCompaction:
		cfg.AfterCompaction = []Hook{h}
	}
	return cfg
}

func TestExecutePostToolUseDoesNotFailClosedOnError(t *testing.T) {
	t.Parallel()

	config := &Config{
		PostToolUse: []MatcherConfig{
			{
				Matcher: "*",
				Hooks: []Hook{
					{Type: HookTypeCommand, Command: "sleep 10", Timeout: 30},
				},
			},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID: "test-session",
		ToolName:  "shell",
		ToolUseID: "test-id",
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	result, err := exec.Dispatch(ctx, EventPostToolUse, input)
	require.NoError(t, err)
	// Post-tool-use is observational only: a failed hook must not block
	// the already-completed tool call.
	assert.True(t, result.Allowed)
}

// TestExecuteBeforeCompactionAllowedByDefault checks that an empty
// observational hook (echo to stdout) does not deny compaction. Plain
// stdout is dropped for before_compaction since the runtime acts on
// Allowed and Summary, not on AdditionalContext.
func TestExecuteBeforeCompactionAllowedByDefault(t *testing.T) {
	t.Parallel()

	config := &Config{
		BeforeCompaction: []Hook{
			{Type: HookTypeCommand, Command: "echo 'about to compact'", Timeout: 5},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID:        "test-session",
		InputTokens:      100_000,
		OutputTokens:     5_000,
		ContextLimit:     128_000,
		CompactionReason: "threshold",
	}

	result, err := exec.Dispatch(t.Context(), EventBeforeCompaction, input)
	require.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Empty(t, result.Summary)
}

// TestExecuteBeforeCompactionBlocksWithExitCode2 pins the contract that a
// before_compaction hook can veto compaction by exiting with code 2.
func TestExecuteBeforeCompactionBlocksWithExitCode2(t *testing.T) {
	t.Parallel()

	config := &Config{
		BeforeCompaction: []Hook{
			{Type: HookTypeCommand, Command: "echo 'no compaction please' >&2; exit 2", Timeout: 5},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID:        "test-session",
		CompactionReason: "manual",
	}

	result, err := exec.Dispatch(t.Context(), EventBeforeCompaction, input)
	require.NoError(t, err)
	assert.False(t, result.Allowed)
	assert.Equal(t, 2, result.ExitCode)
}

// TestExecuteBeforeCompactionSurfacesSummary checks that a before_compaction
// hook returning HookSpecificOutput.summary populates Result.Summary so the
// runtime can apply it verbatim and skip the LLM-based compaction.
func TestExecuteBeforeCompactionSurfacesSummary(t *testing.T) {
	t.Parallel()

	jsonOutput := `{"hook_specific_output":{"hook_event_name":"before_compaction","summary":"hook-supplied summary"}}`
	config := &Config{
		BeforeCompaction: []Hook{
			{Type: HookTypeCommand, Command: "echo '" + jsonOutput + "'", Timeout: 5},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID:        "test-session",
		CompactionReason: "manual",
	}

	result, err := exec.Dispatch(t.Context(), EventBeforeCompaction, input)
	require.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Equal(t, "hook-supplied summary", result.Summary)
}

// TestExecuteBeforeCompactionFirstSummaryWins checks that when multiple
// hooks return a summary, the first non-empty one is kept. Concatenating
// summaries would produce nonsense; clobbering would be order-dependent.
func TestExecuteBeforeCompactionFirstSummaryWins(t *testing.T) {
	t.Parallel()

	first := `{"hook_specific_output":{"hook_event_name":"before_compaction","summary":"first"}}`
	second := `{"hook_specific_output":{"hook_event_name":"before_compaction","summary":"second"}}`
	config := &Config{
		BeforeCompaction: []Hook{
			{Type: HookTypeCommand, Command: "echo '" + first + "'", Timeout: 5},
			{Type: HookTypeCommand, Command: "echo '" + second + "'", Timeout: 5},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	result, err := exec.Dispatch(t.Context(), EventBeforeCompaction, &Input{
		SessionID:        "test-session",
		CompactionReason: "manual",
	})
	require.NoError(t, err)
	assert.True(t, result.Allowed)
	// Hooks run concurrently, so we assert on either-or rather than
	// strict ordering. The contract is "first non-empty wins"; with
	// concurrent execution that means a deterministic single value
	// surfaces (no concatenation) — never both.
	assert.Contains(t, []string{"first", "second"}, result.Summary)
	assert.NotEqual(t, "firstsecond", result.Summary)
	assert.NotEqual(t, "secondfirst", result.Summary)
}

// TestExecuteAfterCompactionIsObservational pins the contract that the
// after_compaction event ignores its hooks' Allowed/Summary verdicts —
// they are surfaced into Result for inspection but do not affect the
// runtime, which calls the hook purely for observability.
func TestExecuteAfterCompactionIsObservational(t *testing.T) {
	t.Parallel()

	config := &Config{
		AfterCompaction: []Hook{
			{Type: HookTypeCommand, Command: "echo 'compaction done'", Timeout: 5},
		},
	}

	exec := NewExecutor(config, t.TempDir(), nil)
	input := &Input{
		SessionID:        "test-session",
		CompactionReason: "threshold",
		Summary:          "earlier summary",
	}

	result, err := exec.Dispatch(t.Context(), EventAfterCompaction, input)
	require.NoError(t, err)
	assert.True(t, result.Allowed)
}

// TestInputCompactionFieldsSerialize pins the wire format for the
// compaction-specific Input fields so external (command-type) hooks can
// rely on it.
func TestInputCompactionFieldsSerialize(t *testing.T) {
	t.Parallel()

	input := &Input{
		SessionID:        "sess-789",
		HookEventName:    EventBeforeCompaction,
		InputTokens:      120_000,
		OutputTokens:     8_000,
		ContextLimit:     200_000,
		CompactionReason: "overflow",
	}

	data, err := input.ToJSON()
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(data, &parsed))

	assert.Equal(t, "sess-789", parsed["session_id"])
	assert.Equal(t, "before_compaction", parsed["hook_event_name"])
	assert.EqualValues(t, 120_000, parsed["input_tokens"])
	assert.EqualValues(t, 8_000, parsed["output_tokens"])
	assert.EqualValues(t, 200_000, parsed["context_limit"])
	assert.Equal(t, "overflow", parsed["compaction_reason"])
	// Summary is omitted on before_compaction inputs (it's only set on
	// after_compaction so handlers receive the produced summary).
	_, hasSummary := parsed["summary"]
	assert.False(t, hasSummary)
}
