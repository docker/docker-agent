package root

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/portcullis"
	"github.com/goccy/go-yaml"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/modelsdev/embedded"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
)

const debugToolConfig = `
agents:
  root:
    model: test
    toolsets:
      - type: think
      - type: filesystem
        tools: [read_file]
  helper:
    model: test
    toolsets:
      - type: filesystem
        tools: [read_multiple_files]
        defer: true
models:
  test:
    provider: openai
    model: gpt-4o
    max_tokens: 100
`

func runDebugTool(t *testing.T, flags *debugFlags, args ...string) (string, error) {
	t.Helper()

	return runDebugToolConfig(t, flags, debugToolConfig, args...)
}

func runDebugToolConfig(t *testing.T, flags *debugFlags, cfg string, args ...string) (string, error) {
	t.Helper()

	out, _, err := runDebugToolConfigOutputs(t, flags, cfg, args...)
	return out, err
}

func runDebugToolConfigOutputs(t *testing.T, flags *debugFlags, cfg string, args ...string) (string, string, error) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte(cfg), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello from a tool"), 0o600))
	flags.runConfig.WorkingDir = dir
	flags.runConfig.EnvProviderOverride = environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test-key"})
	if flags.runConfig.ModelsDevStoreOverride == nil {
		flags.runConfig.ModelsDevStoreOverride = modelsdev.NewDatabaseStore(embedded.Snapshot())
	}

	var out, warnings bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&warnings)
	cmd.SetContext(t.Context())

	err := flags.runDebugToolCommand(cmd, append([]string{path}, args...))
	return out.String(), warnings.String(), err
}

func TestDebugToolCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		flags  debugFlags
		args   []string
		output string
		err    string
	}{
		{
			name:   "default agent",
			args:   []string{"think", `{"thought":"testing tools"}`},
			output: "Thoughts:\ntesting tools\n",
		},
		{
			name:   "default parameters",
			args:   []string{"think"},
			output: "Thoughts:\n\n",
		},
		{
			name:   "working directory",
			args:   []string{"read_file", `{"path":"hello.txt"}`},
			output: "hello from a tool",
		},
		{
			name:   "selected agent and deferred tool",
			flags:  debugFlags{toolAgent: "helper"},
			args:   []string{"read_multiple_files", `{"paths":["hello.txt"]}`},
			output: "=== hello.txt ===\nhello from a tool\n",
		},
		{
			name:  "unknown agent",
			flags: debugFlags{toolAgent: "missing"},
			args:  []string{"think"},
			err:   "agent not found: missing (available agents: root, helper)",
		},
		{
			name: "tool belongs to another agent",
			args: []string{"read_multiple_files"},
			err:  `tool "read_multiple_files" not found for agent "root"`,
		},
		{
			name: "filtered tool",
			args: []string{"write_file", `{"path":"hello.txt","content":"overwrite"}`},
			err:  `tool "write_file" not found for agent "root"`,
		},
		{
			name: "invalid JSON",
			args: []string{"think", `{"thought":`},
			err:  "parameters must be a JSON object",
		},
		{
			name: "array parameters",
			args: []string{"think", `[]`},
			err:  "parameters must be a JSON object",
		},
		{
			name: "null parameters",
			args: []string{"think", `null`},
			err:  "parameters must be a JSON object, not null",
		},
		{
			name: "invalid parameter type",
			args: []string{"think", `{"thought":{}}`},
			err:  `calling tool "think"`,
		},
		{
			name:   "tool error",
			args:   []string{"read_file", `{"path":"missing.txt"}`},
			output: "not found\n",
			err:    `tool "read_file" returned an error for agent "root"`,
		},
	}
	for i := range tests {
		tt := &tests[i]
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, err := runDebugTool(t, &tt.flags, tt.args...)
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
			} else {
				require.NoError(t, err)
			}
			if tt.output != "" {
				assert.Contains(t, out, tt.output)
			} else {
				assert.Empty(t, out)
			}
		})
	}
}

func TestDebugToolCommand_JSON(t *testing.T) {
	t.Parallel()

	for _, isError := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "tool error"}[isError], func(t *testing.T) {
			t.Parallel()

			args := []string{"think", `{"thought":"hello"}`}
			if isError {
				args = []string{"read_file", `{"path":"missing.txt"}`}
			}
			out, err := runDebugTool(t, &debugFlags{toolJSON: true}, args...)
			if isError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			var result tools.ToolCallResult
			require.NoError(t, json.Unmarshal([]byte(out), &result))
			assert.Equal(t, isError, result.IsError)
			assert.NotEmpty(t, result.Output)
		})
	}
}

func TestDebugToolCommand_Arguments(t *testing.T) {
	t.Parallel()

	cmd, _, err := newDebugCmd().Find([]string{"tool"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(t, err)
	require.Equal(t, "tool", cmd.Name())
	for _, args := range [][]string{nil, {"agent.yaml"}, {"agent.yaml", "think", "{}", "extra"}} {
		require.Error(t, cmd.Args(cmd, args))
	}
	assert.NoError(t, cmd.Args(cmd, []string{"agent.yaml", "think"}))
	assert.NoError(t, cmd.Args(cmd, []string{"agent.yaml", "think", "{}"}))
	require.NoError(t, cmd.ParseFlags([]string{"-a", "helper", "--json", "--no-hook"}))
	noHook, err := cmd.Flags().GetBool("no-hook")
	require.NoError(t, err)
	assert.True(t, noHook)
	require.NoError(t, cmd.Help())
	assert.Contains(t, out.String(), "Call a tool of an agent directly")
}

func TestCallDebugTool(t *testing.T) {
	t.Parallel()

	t.Run("call shape and full result", func(t *testing.T) {
		t.Parallel()

		expected := &tools.ToolCallResult{
			Output:            "result",
			Images:            []tools.MediaContent{{Data: "aGVsbG8=", MimeType: "image/png"}},
			StructuredContent: map[string]any{"key": "value"},
		}
		tool := tools.Tool{Name: "test", Handler: func(ctx context.Context, tc tools.ToolCall, rt tools.Runtime) (*tools.ToolCallResult, error) {
			assert.Equal(t, t.Context(), ctx)
			assert.Equal(t, "debug_test", tc.ID)
			assert.Equal(t, tools.ToolType("function"), tc.Type)
			assert.Equal(t, tools.FunctionCall{Name: "test", Arguments: `{"value":42}`}, tc.Function)
			assert.IsType(t, tools.NopRuntime{}, rt)
			return expected, nil
		}}
		result, err := callDebugTool(t.Context(), tool, `{"value":42}`)
		require.NoError(t, err)
		assert.Same(t, expected, result)
	})

	t.Run("canceled before execution", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		tool := tools.Tool{Name: "test", Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			t.Fatal("canceled handler must not run")
			return nil, nil
		}}
		result, err := callDebugTool(ctx, tool, "{}")
		require.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, result)
	})

	t.Run("canceled during execution", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		tool := tools.Tool{Name: "test", Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			cancel()
			return tools.ResultSuccess("ignored cancellation"), nil
		}}
		result, err := callDebugTool(ctx, tool, "{}")
		require.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, result)
	})

	t.Run("expired deadline", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()
		tool := tools.Tool{Name: "test", Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			t.Fatal("expired handler must not run")
			return nil, nil
		}}
		result, err := callDebugTool(ctx, tool, "{}")
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Nil(t, result)
	})

	t.Run("handler error", func(t *testing.T) {
		t.Parallel()

		expected := errors.New("handler failed")
		tool := tools.Tool{Name: "test", Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return nil, expected
		}}
		_, err := callDebugTool(t.Context(), tool, "{}")
		require.ErrorIs(t, err, expected)
	})

	t.Run("nil result", func(t *testing.T) {
		t.Parallel()

		tool := tools.Tool{Name: "test", Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return nil, nil
		}}
		_, err := callDebugTool(t.Context(), tool, "{}")
		require.ErrorContains(t, err, "returned no result")
	})

	t.Run("runtime tool", func(t *testing.T) {
		t.Parallel()

		tool := tools.Tool{Name: "test", RuntimeHandler: "runtime", Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			t.Fatal("runtime handler must not be called")
			return nil, nil
		}}
		_, err := callDebugTool(t.Context(), tool, "{}")
		require.ErrorContains(t, err, "requires an agent runtime")
	})

	t.Run("missing handler", func(t *testing.T) {
		t.Parallel()

		_, err := callDebugTool(t.Context(), tools.Tool{Name: "test"}, "{}")
		require.ErrorContains(t, err, "requires an agent runtime")
	})
}

func TestDebugToolCommand_MCP(t *testing.T) {
	t.Parallel()

	server := mcp.NewServer(&mcp.Implementation{Name: "debug-test", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, args struct {
		Message string `json:"message"`
	},
	) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: args.Message},
				&mcp.ImageContent{Data: []byte("image"), MIMEType: "image/png"},
			},
		}, map[string]string{"message": args.Message}, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil))
	t.Cleanup(httpServer.Close)

	cfg := fmt.Sprintf(`agents:
  root:
    model: test
    toolsets:
      - type: mcp
        name: test
        allow_private_ips: true
        remote:
          url: %s
          transport_type: streamable
models:
  test:
    provider: openai
    model: gpt-4o
    max_tokens: 100
`, httpServer.URL)
	out, err := runDebugToolConfig(t, &debugFlags{toolJSON: true}, cfg, "test_echo", `{"message":"hello MCP"}`)
	require.NoError(t, err)
	var result tools.ToolCallResult
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, "hello MCP", result.Output)
	assert.False(t, result.IsError)
	assert.Equal(t, []tools.MediaContent{{Data: "aW1hZ2U=", MimeType: "image/png"}}, result.Images)
	assert.Equal(t, map[string]any{"message": "hello MCP"}, result.StructuredContent)
}

func TestDebugToolsetsCommand_IncludesDeferredTools(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte(debugToolConfig), 0o600))
	flags := &debugFlags{toolsetsJSON: true}
	flags.runConfig.EnvProviderOverride = environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test-key"})
	flags.runConfig.ModelsDevStoreOverride = modelsdev.NewDatabaseStore(embedded.Snapshot())

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetOut(&out)
	require.NoError(t, flags.runDebugToolsetsCommand(cmd, []string{path}))
	var infos []agentToolsInfo
	require.NoError(t, json.Unmarshal(out.Bytes(), &infos))
	require.Len(t, infos, 2)
	found := false
	for _, info := range infos {
		for _, tool := range info.Tools {
			if tool.Name != "read_multiple_files" {
				continue
			}
			assert.Equal(t, "helper", info.Agent, "deferred tool belongs only to the selected agent")
			assert.NotNil(t, tool.Parameters)
			found = true
		}
	}
	require.True(t, found, "deferred filesystem tool must be discoverable")
}

func TestCallDebugTool_CanceledWriteFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	available, err := filesystem.New(dir).Tools(t.Context())
	require.NoError(t, err)
	index := slices.IndexFunc(available, func(tool tools.Tool) bool { return tool.Name == filesystem.ToolNameWriteFile })
	require.NotEqual(t, -1, index)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = callDebugTool(ctx, available[index], `{"path":"marker","content":"must not be written"}`)
	require.ErrorIs(t, err, context.Canceled)
	assert.NoFileExists(t, filepath.Join(dir, "marker"))
}

func TestDebugToolCommand_BackgroundJobs(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"direct", "deferred", "code mode"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			for _, recall := range []bool{false, true} {
				t.Run(fmt.Sprintf("recall=%t", recall), func(t *testing.T) {
					t.Parallel()

					flags := &debugFlags{}
					flags.runConfig.GlobalCodeMode = mode == "code mode"
					cfg := fmt.Sprintf(`agents:
  root:
    model: test
    toolsets:
      - type: background_jobs
        recall: %t
        defer: %t
models:
  test:
    provider: openai
    model: gpt-4o
    max_tokens: 100
`, recall, mode == "deferred")
					args := []string{"run_background_job", `{"cmd":"echo must-not-run"}`}
					if mode == "code mode" {
						args = []string{"run_tools_with_javascript", `{"script":"try { await run_background_job({cmd: 'echo must-not-run'}); return 'unexpected success'; } catch (error) { return error.message; }"}`}
					}
					out, err := runDebugToolConfig(t, flags, cfg, args...)
					if mode == "code mode" {
						require.NoError(t, err)
						assert.Contains(t, out, "background jobs are not supported by this host")
					} else {
						require.ErrorContains(t, err, "background jobs are not supported by this host")
						assert.Empty(t, out)
					}
				})
			}
		})
	}
}

const debugToolHooksConfig = `
agents:
  root:
    model: test
    toolsets:
      - type: think
      - type: filesystem
        tools: [read_file]
    hooks:
      tool_response_transform:
        - matcher: think|read_file
          hooks:
            - type: command
              command: >-
                printf '%s' '{"hook_specific_output":{"updated_tool_response":"first rewrite"}}'
            - type: command
              command: >-
                cat > hook-input.json;
                printf '%s' '{"hook_specific_output":{"updated_tool_response":"transformed"}}'
        - matcher: other_tool
          hooks:
            - type: command
              command: >-
                printf '%s' '{"hook_specific_output":{"updated_tool_response":"wrong matcher"}}'
      post_tool_use:
        - hooks:
            - type: command
              command: cat > post-tool-input.json
      session_start:
        - type: command
          command: touch session-marker
  helper:
    model: test
    toolsets:
      - type: think
    hooks:
      tool_response_transform:
        - hooks:
            - type: command
              command: >-
                printf '%s' '{"hook_specific_output":{"updated_tool_response":"helper response"}}'
models:
  test:
    provider: openai
    model: gpt-4o
    max_tokens: 100
`

func TestDebugToolCommand_ResponseHooks(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("hook fixtures use POSIX shell commands")
	}

	for _, jsonOutput := range []bool{false, true} {
		for _, toolError := range []bool{false, true} {
			t.Run(fmt.Sprintf("json=%t/error=%t", jsonOutput, toolError), func(t *testing.T) {
				t.Parallel()

				flags := &debugFlags{toolJSON: jsonOutput}
				args := []string{"think", `{"thought":"original"}`}
				category := "think"
				if toolError {
					args = []string{"read_file", `{"path":"missing.txt"}`}
					category = "filesystem"
				}
				out, warnings, err := runDebugToolConfigOutputs(t, flags, debugToolHooksConfig, args...)
				if toolError {
					require.ErrorContains(t, err, "returned an error")
				} else {
					require.NoError(t, err)
				}
				assert.Empty(t, warnings)
				if jsonOutput {
					var result tools.ToolCallResult
					require.NoError(t, json.Unmarshal([]byte(out), &result))
					assert.Equal(t, "transformed", result.Output)
					assert.Equal(t, toolError, result.IsError)
				} else {
					assert.Equal(t, "transformed\n", out)
				}
				data, err := os.ReadFile(filepath.Join(flags.runConfig.WorkingDir, "hook-input.json"))
				require.NoError(t, err)
				var input hooks.Input
				require.NoError(t, json.Unmarshal(data, &input))
				assert.Equal(t, hooks.EventToolResponseTransform, input.HookEventName)
				assert.Equal(t, "root", input.AgentName)
				assert.Equal(t, args[0], input.ToolName)
				assert.Equal(t, category, input.ToolCategory)
				assert.Equal(t, "debug_"+args[0], input.ToolUseID)
				assert.NotEmpty(t, input.SessionID)
				assert.Equal(t, flags.runConfig.WorkingDir, input.Cwd)
				assert.JSONEq(t, args[1], string(mustMarshalJSON(t, input.ToolInput)))
				assert.Equal(t, "first rewrite", input.ToolResponse)
				assert.Equal(t, toolError, input.ToolError)
				postData, err := os.ReadFile(filepath.Join(flags.runConfig.WorkingDir, "post-tool-input.json"))
				require.NoError(t, err)
				var postInput hooks.Input
				require.NoError(t, json.Unmarshal(postData, &postInput))
				assert.Equal(t, hooks.EventPostToolUse, postInput.HookEventName)
				assert.Equal(t, input.SessionID, postInput.SessionID)
				assert.Equal(t, input.ToolUseID, postInput.ToolUseID)
				assert.Equal(t, input.ToolInput, postInput.ToolInput)
				assert.Equal(t, "transformed", postInput.ToolResponse)
				assert.Equal(t, toolError, postInput.ToolError)
				assert.NoFileExists(t, filepath.Join(flags.runConfig.WorkingDir, "session-marker"))
			})
		}
	}
}

func TestDebugToolCommand_NoHook(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("hook fixtures use POSIX shell commands")
	}

	for _, jsonOutput := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%t", jsonOutput), func(t *testing.T) {
			t.Parallel()

			flags := &debugFlags{toolNoHook: true, toolJSON: jsonOutput}
			out, warnings, err := runDebugToolConfigOutputs(t, flags, debugToolHooksConfig, "think", `{"thought":"original"}`)
			require.NoError(t, err)
			assert.Empty(t, warnings)
			assert.Contains(t, out, "original")
			assert.NotContains(t, out, "transformed")
			assert.NoFileExists(t, filepath.Join(flags.runConfig.WorkingDir, "hook-input.json"))
			assert.NoFileExists(t, filepath.Join(flags.runConfig.WorkingDir, "post-tool-input.json"))
		})
	}
}

func TestDebugToolCommand_SelectedAgentHooks(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("hook fixtures use POSIX shell commands")
	}

	flags := &debugFlags{toolAgent: "helper"}
	out, err := runDebugToolConfig(t, flags, debugToolHooksConfig, "think")
	require.NoError(t, err)
	assert.Equal(t, "helper response\n", out)
	assert.NoFileExists(t, filepath.Join(flags.runConfig.WorkingDir, "hook-input.json"))
}

func TestDebugToolCommand_ResponseHookResults(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("hook fixtures use POSIX shell commands")
	}

	for _, tc := range []struct {
		name    string
		command string
		output  string
		warning string
	}{
		{"empty rewrite", `printf '%s' '{"hook_specific_output":{"updated_tool_response":""}}'`, "\n", ""},
		{"no rewrite", "true", "Thoughts:\noriginal\n", ""},
		{"failed hook", "exit 1", "Thoughts:\noriginal\n", "exited with status 1"},
		{"ignored failure", "exit 1", "Thoughts:\noriginal\n", ""},
		{"system message", `printf '%s' '{"system_message":"hook warning","hook_specific_output":{"updated_tool_response":"rewritten"}}'`, "rewritten\n", "hook warning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := strings.ReplaceAll(debugToolConfig, "    model: test\n    toolsets:", fmt.Sprintf("    model: test\n    hooks:\n      tool_response_transform:\n        - hooks:\n            - type: command\n              command: %q\n              on_error: %s\n    toolsets:", tc.command, map[bool]string{true: "ignore", false: "warn"}[tc.name == "ignored failure"]))
			out, warnings, err := runDebugToolConfigOutputs(t, &debugFlags{}, cfg, "think", `{"thought":"original"}`)
			require.NoError(t, err)
			assert.Equal(t, tc.output, out)
			if tc.warning == "" {
				assert.Empty(t, warnings)
			} else {
				assert.Contains(t, warnings, tc.warning)
			}
		})
	}
}

func TestDebugToolCommand_BuiltinResponseHooks(t *testing.T) {
	t.Parallel()

	t.Run("JSON transform", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" {
			t.Skip("hook fixture uses POSIX shell commands")
		}

		cfg := strings.Replace(debugToolConfig, "    model: test", `    model: test
    hooks:
      tool_response_transform:
        - matcher: read_file
          hooks:
            - type: command
              command: >-
                printf '%s' '{"hook_specific_output":{"updated_tool_response":"{\"keep\":42,\"drop\":true}"}}'
            - type: builtin
              command: transform_json
              args: [keep]`, 1)
		out, err := runDebugToolConfig(t, &debugFlags{}, cfg, "read_file", `{"path":"hello.txt"}`)
		require.NoError(t, err)
		assert.JSONEq(t, `{"keep":42}`, out)
	})

	t.Run("agent secret redaction", func(t *testing.T) {
		t.Parallel()

		const secret = "dckr_pat_" + "AAAAAAAAAAAAAAAAAAAAAAAAAAA"
		cfg := strings.Replace(debugToolConfig, "    model: test", "    model: test\n    redact_secrets: true", 1)
		for _, noHook := range []bool{false, true} {
			out, err := runDebugToolConfig(t, &debugFlags{toolNoHook: noHook}, cfg, "think", `{"thought":"`+secret+`"}`)
			require.NoError(t, err)
			if noHook {
				assert.Contains(t, out, secret)
			} else {
				assert.NotContains(t, out, secret)
				assert.Contains(t, out, portcullis.Marker)
			}
		}
	})
}

func TestDispatchDebugToolHook_CanceledBeforeHook(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cfg := &hooks.Config{ToolResponseTransform: []hooks.MatcherConfig{{Hooks: []hooks.Hook{{Command: "true"}}}}}
	a := agent.New("root", "", agent.WithHooks(cfg))
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cancel()
	flags := &debugFlags{}
	executor, err := flags.debugToolHooksExecutor(a, nil)
	require.NoError(t, err)
	_, err = dispatchDebugToolHook(ctx, cmd, executor, hooks.EventToolResponseTransform, &hooks.Input{ToolName: "test"})
	require.ErrorIs(t, err, context.Canceled)
}

func mustMarshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}

func TestDebugToolCommand_LargeResponse(t *testing.T) {
	t.Parallel()
	for _, noHook := range []bool{false, true} {
		t.Run(fmt.Sprintf("no-hook=%t", noHook), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "large.txt")
			payload := strings.Repeat("0123456789\n", 7000)
			require.NoError(t, os.WriteFile(path, []byte(payload), 0o600))
			out, err := runDebugTool(t, &debugFlags{toolNoHook: noHook}, "read_file", string(mustMarshalJSON(t, map[string]string{"path": path})))
			require.NoError(t, err)
			if noHook {
				assert.Contains(t, out, payload)
				assert.NotContains(t, out, "Tool call result was too large")
				return
			}
			assert.LessOrEqual(t, len(out), 50*1024+1)
			_, notice, ok := strings.Cut(out, "The full result is available in a file: ")
			require.True(t, ok)
			spillPath, _, ok := strings.Cut(notice, "\n")
			require.True(t, ok)
			t.Cleanup(func() { assert.NoError(t, os.RemoveAll(filepath.Dir(spillPath))) })
			data, err := os.ReadFile(spillPath)
			require.NoError(t, err)
			assert.Contains(t, string(data), payload)
		})
	}
}

func TestDebugToolHooksExecutor_PreservesConfig(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("hook fixtures use POSIX shell commands")
	}

	cfg := &hooks.Config{ToolResponseTransform: []hooks.MatcherConfig{{Hooks: []hooks.Hook{{
		Type: hooks.HookTypeCommand, Command: `printf '%s' '{"hook_specific_output":{"updated_tool_response":"rewritten"}}'`,
	}}}}}
	a := agent.New("root", "", agent.WithHooks(cfg), agent.WithRedactSecrets(true))
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	flags := &debugFlags{}
	flags.runConfig.WorkingDir = t.TempDir()
	executor, err := flags.debugToolHooksExecutor(a, nil)
	require.NoError(t, err)
	result, err := dispatchDebugToolHook(t.Context(), cmd, executor, hooks.EventToolResponseTransform, &hooks.Input{ToolName: "test", ToolResponse: "original"})
	require.NoError(t, err)
	require.NotNil(t, result.UpdatedToolResponse)
	assert.Equal(t, "rewritten", *result.UpdatedToolResponse)
	assert.Len(t, cfg.ToolResponseTransform, 1)
	assert.Empty(t, cfg.SessionEnd)
}

func TestDispatchDebugToolHook_CanceledDuringHook(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("hook fixtures use POSIX shell commands")
	}
	for _, event := range []hooks.EventType{hooks.EventToolInputTransform, hooks.EventToolResponseTransform, hooks.EventPostToolUse} {
		t.Run(string(event), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			matchers := []hooks.MatcherConfig{{Hooks: []hooks.Hook{{Type: hooks.HookTypeCommand, Command: "touch hook-started; exec sleep 60"}}}}
			cfg := &hooks.Config{}
			switch event {
			case hooks.EventToolInputTransform:
				cfg.ToolInputTransform = matchers
			case hooks.EventToolResponseTransform:
				cfg.ToolResponseTransform = matchers
			case hooks.EventPostToolUse:
				cfg.PostToolUse = matchers
			}
			a := agent.New("root", "", agent.WithHooks(cfg))
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetContext(ctx)
			cmd.SetErr(&out)
			flags := &debugFlags{}
			flags.runConfig.WorkingDir = t.TempDir()
			executor, err := flags.debugToolHooksExecutor(a, nil)
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() {
				_, err := dispatchDebugToolHook(ctx, cmd, executor, event, &hooks.Input{ToolName: "test"})
				done <- err
			}()
			require.Eventually(t, func() bool {
				_, err := os.Stat(filepath.Join(flags.runConfig.WorkingDir, "hook-started"))
				return err == nil
			}, 5*time.Second, 10*time.Millisecond)
			cancel()
			err = <-done
			require.ErrorIs(t, err, context.Canceled)
			assert.Empty(t, out.String())
		})
	}
}

func debugToolConfigWithHooks(t *testing.T, cfg *hooks.Config) string {
	t.Helper()
	data, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	indented := "    hooks:\n      " + strings.ReplaceAll(strings.TrimSuffix(string(data), "\n"), "\n", "\n      ")
	return strings.Replace(debugToolConfig, "    model: test", "    model: test\n"+indented, 1)
}

func debugHookCommand(output, capture string) hooks.Hook {
	var command string
	if runtime.GOOS == "windows" {
		if capture != "" {
			command = "[IO.File]::WriteAllText('" + capture + "', [Console]::In.ReadToEnd()); "
		}
		command += "[Console]::Write('" + output + "')"
	} else {
		if capture != "" {
			command = "cat > " + capture + "; "
		}
		command += "printf '%s' '" + output + "'"
	}
	return hooks.Hook{Type: hooks.HookTypeCommand, Command: command}
}

func readDebugHookInput(t *testing.T, dir, name string) hooks.Input {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)
	var input hooks.Input
	require.NoError(t, json.Unmarshal(data, &input))
	return input
}

func TestDebugToolCommand_InputAndPostHooks(t *testing.T) {
	t.Parallel()

	cfg := debugToolConfigWithHooks(t, &hooks.Config{
		ToolInputTransform: []hooks.MatcherConfig{
			{Matcher: "think", Hooks: []hooks.Hook{
				debugHookCommand(`{"hook_specific_output":{"updated_input":{"thought":"first"}}}`, "before.json"),
				debugHookCommand(`{"hook_specific_output":{"updated_input":{"thought":"rewritten"}}}`, "rewrite.json"),
			}},
			{Matcher: "other_tool", Hooks: []hooks.Hook{debugHookCommand(`{"decision":"block"}`, "wrong-input.json")}},
		},
		ToolResponseTransform: []hooks.MatcherConfig{{Matcher: "think", Hooks: []hooks.Hook{
			debugHookCommand(`{"hook_specific_output":{"updated_tool_response":"transformed"}}`, "response.json"),
		}}},
		PostToolUse: []hooks.MatcherConfig{
			{Matcher: "think", Hooks: []hooks.Hook{debugHookCommand(`{"system_message":"post hook ran"}`, "post.json")}},
			{Matcher: "other_tool", Hooks: []hooks.Hook{debugHookCommand(`{"decision":"block"}`, "wrong-post.json")}},
		},
		PreToolUse: []hooks.MatcherConfig{{Hooks: []hooks.Hook{debugHookCommand(`{"decision":"block"}`, "pre-tool.json")}}},
		ToolGuard:  []hooks.MatcherConfig{{Hooks: []hooks.Hook{debugHookCommand(`{"decision":"block"}`, "guard.json")}}},
	})
	for _, noHook := range []bool{false, true} {
		for _, jsonOutput := range []bool{false, true} {
			t.Run(fmt.Sprintf("no-hook=%t/json=%t", noHook, jsonOutput), func(t *testing.T) {
				t.Parallel()
				flags := &debugFlags{toolNoHook: noHook, toolJSON: jsonOutput}
				out, warnings, err := runDebugToolConfigOutputs(t, flags, cfg, "think", `{"thought":"original","preserved":42}`)
				require.NoError(t, err)
				output := out
				if jsonOutput {
					var result tools.ToolCallResult
					require.NoError(t, json.Unmarshal([]byte(out), &result))
					output = result.Output
				}
				dir := flags.runConfig.WorkingDir
				if noHook {
					assert.Contains(t, output, "original")
					assert.Empty(t, warnings)
					for _, path := range []string{"before.json", "rewrite.json", "response.json", "post.json"} {
						assert.NoFileExists(t, filepath.Join(dir, path))
					}
				} else {
					assert.Equal(t, "transformed", strings.TrimSpace(output))
					assert.Contains(t, warnings, "post hook ran")
					before := readDebugHookInput(t, dir, "before.json")
					rewrite := readDebugHookInput(t, dir, "rewrite.json")
					response := readDebugHookInput(t, dir, "response.json")
					post := readDebugHookInput(t, dir, "post.json")
					assert.Equal(t, "original", before.ToolInput["thought"])
					assert.Equal(t, "first", rewrite.ToolInput["thought"])
					assert.Equal(t, "rewritten", response.ToolInput["thought"])
					assert.InDelta(t, 42, response.ToolInput["preserved"], 0)
					assert.Equal(t, "Thoughts:\nrewritten", response.ToolResponse)
					assert.Equal(t, "transformed", post.ToolResponse)
					assert.Equal(t, response.ToolInput, post.ToolInput)
					assert.Equal(t, hooks.EventToolInputTransform, before.HookEventName)
					assert.Equal(t, hooks.EventPostToolUse, post.HookEventName)
					for _, input := range []hooks.Input{before, rewrite, response, post} {
						assert.Equal(t, before.SessionID, input.SessionID)
						assert.NotEmpty(t, input.SessionID)
						assert.Equal(t, "root", input.AgentName)
						assert.Equal(t, "think", input.ToolName)
						assert.Equal(t, "think", input.ToolCategory)
						assert.Equal(t, "debug_think", input.ToolUseID)
					}
				}
				for _, path := range []string{"pre-tool.json", "guard.json", "wrong-input.json", "wrong-post.json"} {
					assert.NoFileExists(t, filepath.Join(dir, path))
				}
			})
		}
	}
}

func TestDebugToolCommand_InputHookBlocksExecution(t *testing.T) {
	t.Parallel()

	for _, output := range []string{`{"decision":"block","reason":"policy denial"}`, `{"continue":false,"stop_reason":"policy denial"}`} {
		t.Run(output, func(t *testing.T) {
			t.Parallel()
			cfg := debugToolConfigWithHooks(t, &hooks.Config{
				ToolInputTransform: []hooks.MatcherConfig{{Hooks: []hooks.Hook{debugHookCommand(output, "before.json")}}},
				PostToolUse:        []hooks.MatcherConfig{{Hooks: []hooks.Hook{debugHookCommand(`{}`, "post.json")}}},
			})
			cfg = strings.Replace(cfg, "tools: [read_file]", "tools: [read_file, write_file]", 1)
			flags := &debugFlags{}
			out, err := runDebugToolConfig(t, flags, cfg, "write_file", `{"path":"marker","content":"must not run"}`)
			require.ErrorContains(t, err, "blocked by a tool_input_transform hook: policy denial")
			assert.Empty(t, out)
			assert.NoFileExists(t, filepath.Join(flags.runConfig.WorkingDir, "marker"))
			assert.NoFileExists(t, filepath.Join(flags.runConfig.WorkingDir, "post.json"))
		})
	}
}

func TestDebugToolCommand_PostHookBlocksAfterOutput(t *testing.T) {
	t.Parallel()

	cfg := debugToolConfigWithHooks(t, &hooks.Config{
		PostToolUse: []hooks.MatcherConfig{{Hooks: []hooks.Hook{debugHookCommand(`{"decision":"block","reason":"post denial"}`, "post.json")}}},
	})
	for _, jsonOutput := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%t", jsonOutput), func(t *testing.T) {
			t.Parallel()
			flags := &debugFlags{toolJSON: jsonOutput}
			out, err := runDebugToolConfig(t, flags, cfg, "think", `{"thought":"already executed"}`)
			require.ErrorContains(t, err, "blocked by a post_tool_use hook: post denial")
			if jsonOutput {
				var result tools.ToolCallResult
				require.NoError(t, json.Unmarshal([]byte(out), &result))
				assert.Equal(t, "Thoughts:\nalready executed", result.Output)
				assert.False(t, result.IsError)
			} else {
				assert.Equal(t, "Thoughts:\nalready executed\n", out)
			}
		})
	}
}

func TestDebugToolCommand_PostHookOnHandlerError(t *testing.T) {
	t.Parallel()

	cfg := debugToolConfigWithHooks(t, &hooks.Config{
		ToolResponseTransform: []hooks.MatcherConfig{{Hooks: []hooks.Hook{debugHookCommand(`{"hook_specific_output":{"updated_tool_response":"sanitized error"}}`, "")}}},
		PostToolUse:           []hooks.MatcherConfig{{Hooks: []hooks.Hook{debugHookCommand(`{}`, "post.json")}}},
	})
	flags := &debugFlags{}
	out, err := runDebugToolConfig(t, flags, cfg, "think", `{"thought":{}}`)
	require.ErrorContains(t, err, `calling tool "think"`)
	assert.Empty(t, out)
	post := readDebugHookInput(t, flags.runConfig.WorkingDir, "post.json")
	assert.True(t, post.ToolError)
	assert.Equal(t, "sanitized error", post.ToolResponse)
}

func TestDebugToolCommand_InputAndPostHookErrorPolicies(t *testing.T) {
	t.Parallel()

	for _, event := range []hooks.EventType{hooks.EventToolInputTransform, hooks.EventPostToolUse} {
		for _, policy := range []string{"warn", "ignore", "block"} {
			t.Run(string(event)+"/"+policy, func(t *testing.T) {
				t.Parallel()
				matcher := []hooks.MatcherConfig{{Hooks: []hooks.Hook{{Type: hooks.HookTypeCommand, Command: "exit 1", OnError: policy}}}}
				cfg := &hooks.Config{}
				if event == hooks.EventToolInputTransform {
					cfg.ToolInputTransform = matcher
				} else {
					cfg.PostToolUse = matcher
				}
				out, warnings, err := runDebugToolConfigOutputs(t, &debugFlags{}, debugToolConfigWithHooks(t, cfg), "think", `{"thought":"executed"}`)
				if policy == "block" {
					require.ErrorContains(t, err, "blocked by a "+string(event)+" hook")
				} else {
					require.NoError(t, err)
				}
				if policy == "block" && event == hooks.EventToolInputTransform {
					assert.Empty(t, out)
				} else {
					assert.Contains(t, out, "executed")
				}
				if policy == "warn" {
					assert.Contains(t, warnings, "exited with status 1")
				} else {
					assert.Empty(t, warnings)
				}
			})
		}
	}
}

func TestTransformDebugToolInput_PreservesNumbers(t *testing.T) {
	t.Parallel()
	cfg := &hooks.Config{ToolInputTransform: []hooks.MatcherConfig{{Hooks: []hooks.Hook{debugHookCommand(`{"hook_specific_output":{"updated_input":{"message":"rewritten"}}}`, "")}}}}
	a := agent.New("root", "", agent.WithHooks(cfg))
	flags := &debugFlags{}
	flags.runConfig.WorkingDir = t.TempDir()
	executor, err := flags.debugToolHooksExecutor(a, nil)
	require.NoError(t, err)
	arguments := `{"value":9007199254740993,"message":"original"}`
	var params map[string]any
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&params))
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	updated, err := transformDebugToolInput(t.Context(), cmd, executor, &hooks.Input{ToolName: "test", ToolInput: params}, arguments)
	require.NoError(t, err)
	tool := tools.Tool{Name: "test", Handler: func(_ context.Context, call tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
		var args struct {
			Value   int64  `json:"value"`
			Message string `json:"message"`
		}
		require.NoError(t, json.Unmarshal([]byte(call.Function.Arguments), &args))
		assert.Equal(t, int64(9007199254740993), args.Value)
		assert.Equal(t, "rewritten", args.Message)
		return tools.ResultSuccess("ok"), nil
	}}
	_, err = callDebugTool(t.Context(), tool, updated)
	require.NoError(t, err)
}

func TestTransformDebugToolInput_RedactionPreservesNestedNumbers(t *testing.T) {
	t.Parallel()

	a := agent.New("root", "", agent.WithRedactSecrets(true))
	flags := &debugFlags{}
	flags.runConfig.WorkingDir = t.TempDir()
	executor, err := flags.debugToolHooksExecutor(a, nil)
	require.NoError(t, err)
	arguments := `{"record":{"value":9007199254740993,"secret":"` + "dckr_pat_" + `AAAAAAAAAAAAAAAAAAAAAAAAAAA"}}`
	var params map[string]any
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&params))
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetErr(&bytes.Buffer{})
	updated, err := transformDebugToolInput(t.Context(), cmd, executor, &hooks.Input{ToolName: "test", ToolInput: params}, arguments)
	require.NoError(t, err)
	var args struct {
		Record struct {
			Value  int64  `json:"value"`
			Secret string `json:"secret"`
		} `json:"record"`
	}
	require.NoError(t, json.Unmarshal([]byte(updated), &args))
	assert.Equal(t, int64(9007199254740993), args.Record.Value)
	assert.Equal(t, portcullis.Marker, args.Record.Secret)
}

func TestDebugToolUsesEmbeddedCatalog(t *testing.T) {
	t.Parallel()
	flags := &debugFlags{}
	_, err := runDebugTool(t, flags, "think")
	require.NoError(t, err)
	require.NotNil(t, flags.runConfig.ModelsDevStoreOverride)
	db, err := flags.runConfig.ModelsDevStoreOverride.GetDatabase(t.Context())
	require.NoError(t, err)
	assert.Same(t, embedded.Snapshot(), db)
}

func TestDebugToolPreservesCatalogOverride(t *testing.T) {
	t.Parallel()
	store := modelsdev.NewDatabaseStore(&modelsdev.Database{})
	flags := &debugFlags{}
	flags.runConfig.ModelsDevStoreOverride = store
	_, err := runDebugTool(t, flags, "think")
	require.NoError(t, err)
	assert.Same(t, store, flags.runConfig.ModelsDevStoreOverride)
}
