package root

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"uuid"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/backgroundjobs"
)

func (f *debugFlags) runDebugToolCommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := backgroundjobs.WithoutBackgroundJobs(cmd.Context())
	// Tool parameters may contain secrets; keep them out of command telemetry.
	telemetry.TrackCommand(ctx, "debug", []string{"tool"})
	defer func() {
		if commandErr != nil {
			telemetry.TrackCommandError(ctx, "debug", []string{"tool"}, errors.New("tool invocation failed"))
		}
	}()

	arguments := "{}"
	if len(args) == 3 {
		arguments = args[2]
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal([]byte(arguments), &params); err != nil {
		return fmt.Errorf("parameters must be a JSON object: %w", err)
	}
	if params == nil {
		return errors.New("parameters must be a JSON object, not null")
	}

	loaded, err := f.loadTeamWithConfig(ctx, args[0])
	if err != nil {
		return err
	}
	t := loaded.Team
	defer stopToolSets(ctx, t)

	a, err := t.AgentOrDefault(f.toolAgent)
	if err != nil {
		return err
	}

	// Include deferred tools: activation would otherwise be lost between CLI calls.
	available, err := a.ToolsWithCatalog(ctx)
	for _, warning := range a.DrainWarnings() {
		fmt.Fprintln(cmd.ErrOrStderr(), "Warning:", warning)
	}
	if err != nil {
		return fmt.Errorf("listing tools for agent %q: %w", a.Name(), err)
	}

	index := slices.IndexFunc(available, func(tool tools.Tool) bool { return tool.Name == args[1] })
	if index < 0 {
		return fmt.Errorf("tool %q not found for agent %q; use 'debug toolsets --json' to list tools", args[1], a.Name())
	}

	tool := available[index]
	if tool.RuntimeHandler != "" || tool.Handler == nil {
		return fmt.Errorf("tool %q requires an agent runtime and cannot be called directly", tool.Name)
	}
	var executor *hooks.Executor
	var input *hooks.Input
	if !f.toolNoHook {
		executor, err = f.debugToolHooksExecutor(a, loaded.ProviderRegistry)
		if err != nil {
			return err
		}
		var toolInput map[string]any
		decoder := json.NewDecoder(strings.NewReader(arguments))
		decoder.UseNumber()
		if err := decoder.Decode(&toolInput); err != nil {
			return fmt.Errorf("decoding tool input for hooks: %w", err)
		}
		// Retain limiter spill files so printed paths remain usable after this command exits.
		input = &hooks.Input{
			SessionID:    "debug_" + uuid.NewV4().String(),
			AgentName:    a.Name(),
			ToolCategory: tool.Category,
			ToolName:     tool.Name,
			ToolUseID:    "debug_" + tool.Name,
			ToolInput:    toolInput,
		}
		arguments, err = transformDebugToolInput(ctx, cmd, executor, input, arguments)
		if err != nil {
			return err
		}
	}

	result, callErr := callDebugTool(ctx, tool, arguments)
	if callErr != nil {
		if executor == nil || ctx.Err() != nil {
			return callErr
		}
		result = tools.ResultError(callErr.Error())
	}
	var postErr error
	if executor != nil {
		input.ToolResponse = result.Output
		input.ToolError = result.IsError
		transformed, err := dispatchDebugToolHook(ctx, cmd, executor, hooks.EventToolResponseTransform, input)
		if err != nil {
			return err
		}
		if transformed.UpdatedToolResponse != nil {
			result.Output = *transformed.UpdatedToolResponse
		}
		input.ToolResponse = result.Output
		_, postErr = dispatchDebugToolHook(ctx, cmd, executor, hooks.EventPostToolUse, input)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if callErr != nil {
		return errors.Join(callErr, postErr)
	}

	if f.toolJSON {
		err = encodeJSON(cmd, result)
	} else {
		_, err = fmt.Fprintln(cmd.OutOrStdout(), result.Output)
	}
	if err != nil {
		return err
	}
	if result.IsError {
		return errors.Join(fmt.Errorf("tool %q returned an error for agent %q", args[1], a.Name()), postErr)
	}
	return postErr
}

func (f *debugFlags) debugToolHooksExecutor(a *agent.Agent, providers *provider.Registry) (*hooks.Executor, error) {
	workingDir := f.runConfig.WorkingDir
	if workingDir == "" {
		var err error
		workingDir, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolving hook working directory: %w", err)
		}
	}

	cfg := &hooks.Config{}
	if configured := a.Hooks(); configured != nil {
		cfg.ToolInputTransform = slices.Clone(configured.ToolInputTransform)
		cfg.ToolResponseTransform = slices.Clone(configured.ToolResponseTransform)
		cfg.PostToolUse = slices.Clone(configured.PostToolUse)
	}
	cfg = builtins.ApplyAgentDefaults(cfg, builtins.AgentDefaults{RedactSecrets: a.RedactSecrets()})
	registry := hooks.NewRegistry()
	if err := builtins.Register(registry); err != nil {
		return nil, fmt.Errorf("registering builtin hooks: %w", err)
	}
	runtime.RegisterModelHook(registry, providers)
	return hooks.NewExecutorWithRegistry(cfg, workingDir, nil, registry), nil
}

func transformDebugToolInput(ctx context.Context, cmd *cobra.Command, executor *hooks.Executor, input *hooks.Input, arguments string) (string, error) {
	transformed, err := dispatchDebugToolHook(ctx, cmd, executor, hooks.EventToolInputTransform, input)
	if err != nil {
		return "", err
	}
	if transformed.ModifiedInput == nil {
		return arguments, nil
	}
	updated, err := json.Marshal(transformed.ModifiedInput)
	if err != nil {
		return "", fmt.Errorf("encoding transformed tool input: %w", err)
	}
	input.ToolInput = transformed.ModifiedInput
	return string(updated), nil
}

func dispatchDebugToolHook(ctx context.Context, cmd *cobra.Command, executor *hooks.Executor, event hooks.EventType, input *hooks.Input) (*hooks.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result, err := executor.Dispatch(ctx, event, input)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("executing %s hook for tool %q: %w", event, input.ToolName, err)
	}
	if result.SystemMessage != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), "Warning:", result.SystemMessage)
	}
	if !result.Allowed {
		message := fmt.Sprintf("tool %q blocked by a %s hook", input.ToolName, event)
		if reason := strings.TrimSpace(result.Message); reason != "" {
			message += ": " + reason
		}
		return nil, errors.New(message)
	}
	return result, nil
}

func callDebugTool(ctx context.Context, tool tools.Tool, arguments string) (*tools.ToolCallResult, error) {
	if tool.RuntimeHandler != "" || tool.Handler == nil {
		return nil, fmt.Errorf("tool %q requires an agent runtime and cannot be called directly", tool.Name)
	}
	toolCall := tools.ToolCall{
		ID:   "debug_" + tool.Name,
		Type: "function",
		Function: tools.FunctionCall{
			Name:      tool.Name,
			Arguments: arguments,
		},
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result, err := tool.Handler(ctx, toolCall, tools.NopRuntime{})
	if err != nil {
		return nil, fmt.Errorf("calling tool %q: %w", tool.Name, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("tool %q returned no result", tool.Name)
	}
	return result, nil
}
