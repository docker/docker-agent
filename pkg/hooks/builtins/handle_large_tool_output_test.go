package builtins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
)

func TestHandleLargeToolOutputPassThroughUnderThreshold(t *testing.T) {
	t.Parallel()

	cfg := toolOutputConfig{Threshold: 10000}
	args, _ := json.Marshal(cfg)

	out, err := handleLargeToolOutput(t.Context(), &hooks.Input{
		HookEventName: hooks.EventToolResponseTransform,
		ToolName:      "mcp_tool",
		ToolUseID:     "use_123",
		SessionID:     "session_456",
		ToolResponse:  strings.Repeat("a", 1000),
	}, []string{string(args)})
	require.NoError(t, err)
	assert.Nil(t, out, "response under threshold must pass through unchanged")
}

func TestHandleLargeToolOutputSavesToDiskOverThreshold(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := toolOutputConfig{Threshold: 100, OutputDir: tmpDir}
	args, _ := json.Marshal(cfg)

	largeResponse := strings.Repeat("x", 500)

	out, err := handleLargeToolOutput(t.Context(), &hooks.Input{
		HookEventName: hooks.EventToolResponseTransform,
		ToolName:      "mcp_tool",
		ToolUseID:     "use_789",
		SessionID:     "session_abc",
		ToolResponse:  largeResponse,
	}, []string{string(args)})
	require.NoError(t, err)
	require.NotNil(t, out, "response over threshold must produce output")
	require.NotNil(t, out.HookSpecificOutput)
	require.NotNil(t, out.HookSpecificOutput.UpdatedToolResponse)

	pointer := *out.HookSpecificOutput.UpdatedToolResponse
	assert.Contains(t, pointer, "mcp_tool response: 500 chars")
	assert.Contains(t, pointer, tmpDir)
	assert.Contains(t, pointer, "session_abc_use_789.txt")
	assert.Contains(t, pointer, "Use shell tool to read: cat")

	filePath := filepath.Join(tmpDir, "session_abc_use_789.txt")
	data, err := os.ReadFile(filePath)
	require.NoError(t, err)
	assert.Equal(t, largeResponse, string(data))
}

func TestHandleLargeToolOutputNoArgsUsesDefaultThreshold(t *testing.T) {
	t.Parallel()

	out, err := handleLargeToolOutput(t.Context(), &hooks.Input{
		HookEventName: hooks.EventToolResponseTransform,
		ToolName:      "mcp_tool",
		ToolUseID:     "use_123",
		SessionID:     "session_456",
		ToolResponse:  strings.Repeat("x", 50000),
	}, nil)
	require.NoError(t, err)
	assert.NotNil(t, out, "no args means default threshold (30000) is used — response 50000 > 30000, so it must be processed")
}

func TestHandleLargeToolOutputFallsBackToTempDir(t *testing.T) {
	t.Parallel()

	cfg := toolOutputConfig{Threshold: 50}
	args, _ := json.Marshal(cfg)

	out, err := handleLargeToolOutput(t.Context(), &hooks.Input{
		HookEventName: hooks.EventToolResponseTransform,
		ToolName:      "mcp_tool",
		ToolUseID:     "use_999",
		SessionID:     "session_xyz",
		ToolResponse:  strings.Repeat("y", 500),
	}, []string{string(args)})
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput)
	require.NotNil(t, out.HookSpecificOutput.UpdatedToolResponse)

	pointer := *out.HookSpecificOutput.UpdatedToolResponse
	tmpDir := os.TempDir()
	assert.Contains(t, pointer, tmpDir, "must fall back to os.TempDir() when output_dir not set")
}

func TestHandleLargeToolOutputCustomPreviewSize(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := toolOutputConfig{Threshold: 50, OutputDir: tmpDir, PreviewSize: 100}
	args, _ := json.Marshal(cfg)

	response := strings.Repeat("z", 500)

	out, err := handleLargeToolOutput(t.Context(), &hooks.Input{
		HookEventName: hooks.EventToolResponseTransform,
		ToolName:      "mcp_tool",
		ToolUseID:     "use_preview",
		SessionID:     "session_preview",
		ToolResponse:  response,
	}, []string{string(args)})
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput)
	require.NotNil(t, out.HookSpecificOutput.UpdatedToolResponse)

	pointer := *out.HookSpecificOutput.UpdatedToolResponse
	assert.Contains(t, pointer, "First 100 chars:")

	parts := strings.Split(pointer, "First 100 chars:\n")
	require.Len(t, parts, 2, "pointer must have preview section")
	previewContent := strings.Split(parts[1], "\n\n[Use shell")[0]
	assert.Len(t, previewContent, 100, "preview content must be exactly 100 chars")
}

func TestHandleLargeToolOutputNonToolResponseTransformEventIsNoOp(t *testing.T) {
	t.Parallel()

	out, err := handleLargeToolOutput(t.Context(), &hooks.Input{
		HookEventName: hooks.EventPreToolUse,
		ToolName:      "shell",
		ToolResponse:  strings.Repeat("x", 10000),
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, out, "non tool_response_transform event must be no-op")
}

func TestHandleLargeToolOutputNilInput(t *testing.T) {
	t.Parallel()

	out, err := handleLargeToolOutput(t.Context(), nil, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestHandleLargeToolOutputNonStringResponseIsNoOp(t *testing.T) {
	t.Parallel()

	out, err := handleLargeToolOutput(t.Context(), &hooks.Input{
		HookEventName: hooks.EventToolResponseTransform,
		ToolName:      "mcp_tool",
		ToolResponse:  map[string]any{"structured": "payload"},
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, out, "non-string ToolResponse must be no-op")
}

func TestHandleLargeToolOutputEmptyStringResponseIsNoOp(t *testing.T) {
	t.Parallel()

	out, err := handleLargeToolOutput(t.Context(), &hooks.Input{
		HookEventName: hooks.EventToolResponseTransform,
		ToolName:      "mcp_tool",
		ToolResponse:  "",
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, out, "empty string response must be no-op")
}

func TestHandleLargeToolOutputWriteFailureIsPropagated(t *testing.T) {
	t.Parallel()

	cfg := toolOutputConfig{Threshold: 10, OutputDir: "/nonexistent/path"}
	args, _ := json.Marshal(cfg)

	_, err := handleLargeToolOutput(t.Context(), &hooks.Input{
		HookEventName: hooks.EventToolResponseTransform,
		ToolName:      "mcp_tool",
		ToolUseID:     "use_err",
		SessionID:     "session_err",
		ToolResponse:  strings.Repeat("x", 500),
	}, []string{string(args)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create output directory")
}

func TestHandleLargeToolOutputIsRegistered(t *testing.T) {
	t.Parallel()

	reg := hooks.NewRegistry()
	require.NoError(t, Register(reg))

	handler, ok := reg.LookupBuiltin(HandleLargeToolOutput)
	require.Truef(t, ok, "builtin %q must be registered", HandleLargeToolOutput)

	cfg := toolOutputConfig{Threshold: 50}
	args, _ := json.Marshal(cfg)

	out, err := handler(t.Context(), &hooks.Input{
		HookEventName: hooks.EventToolResponseTransform,
		ToolName:      "mcp_tool",
		ToolUseID:     "use_reg",
		SessionID:     "session_reg",
		ToolResponse:  strings.Repeat("x", 500),
	}, []string{string(args)})
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput)
	require.NotNil(t, out.HookSpecificOutput.UpdatedToolResponse)
	assert.Contains(t, *out.HookSpecificOutput.UpdatedToolResponse, "mcp_tool")
}

func TestApplyAgentDefaultsInjectsHandleLargeToolOutput(t *testing.T) {
	t.Parallel()

	cfg := ApplyAgentDefaults(nil, AgentDefaults{
		HandleLargeToolOutput: &latest.HandleLargeToolOutputConfig{Enabled: true},
	})
	require.NotNil(t, cfg)
	require.Len(t, cfg.ToolResponseTransform, 1)
	assert.Equal(t, "*", cfg.ToolResponseTransform[0].Matcher)
	require.Len(t, cfg.ToolResponseTransform[0].Hooks, 1)
	assert.Equal(t, hooks.HookTypeBuiltin, cfg.ToolResponseTransform[0].Hooks[0].Type)
	assert.Equal(t, HandleLargeToolOutput, cfg.ToolResponseTransform[0].Hooks[0].Command)
}

func TestApplyAgentDefaultsDoesNotInjectWhenDisabled(t *testing.T) {
	t.Parallel()

	cfg := ApplyAgentDefaults(nil, AgentDefaults{
		HandleLargeToolOutput: &latest.HandleLargeToolOutputConfig{Enabled: false},
	})
	assert.Nil(t, cfg, "disabled config must not inject hooks (returns nil)")
}

func TestApplyAgentDefaultsDoesNotInjectWhenNil(t *testing.T) {
	t.Parallel()

	cfg := ApplyAgentDefaults(nil, AgentDefaults{
		HandleLargeToolOutput: nil,
	})
	assert.Nil(t, cfg, "nil config must return nil (no hooks to inject)")
}

func TestHandleLargeToolOutputPathTraversalIsBlocked(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := toolOutputConfig{Threshold: 50, OutputDir: tmpDir}
	args, _ := json.Marshal(cfg)

	out, err := handleLargeToolOutput(t.Context(), &hooks.Input{
		HookEventName: hooks.EventToolResponseTransform,
		ToolName:      "mcp_tool",
		ToolUseID:     "../../../etc/cron.d/malicious",
		SessionID:     "session_../../../tmp",
		ToolResponse:  strings.Repeat("z", 500),
	}, []string{string(args)})
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput)
	require.NotNil(t, out.HookSpecificOutput.UpdatedToolResponse)

	pointer := *out.HookSpecificOutput.UpdatedToolResponse
	assert.Contains(t, pointer, tmpDir, "path must resolve to configured output directory")
	assert.Contains(t, pointer, "__", ".. must be replaced to prevent traversal")

	parentDir, err := filepath.Abs("..")
	require.NoError(t, err)
	_ = parentDir
	assert.NotContains(t, pointer, "/../", "no path traversal sequences allowed")
}

func TestSanitizeFilename(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple", "session123", "session123"},
		{"with slash", "session/123", "session_123"},
		{"with backslash", "session\\123", "session_123"},
		{"path traversal", "../../../etc/passwd", "__etc_passwd"},
		{"mixed", "path/to/../../../etc", "path_to____etc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeFilename(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}
