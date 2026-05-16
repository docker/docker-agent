package shell

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/shellpath"
	"github.com/docker/docker-agent/pkg/tools"
)

type ScriptShellTool struct {
	shellTools map[string]latest.ScriptShellToolConfig
	env        []string
}

// Verify interface compliance
var (
	_ tools.ToolSet      = (*ScriptShellTool)(nil)
	_ tools.Instructable = (*ScriptShellTool)(nil)
)

func NewScriptShellTool(shellTools map[string]latest.ScriptShellToolConfig, env []string) (*ScriptShellTool, error) {
	for toolName, tool := range shellTools {
		if err := validateConfig(toolName, tool); err != nil {
			return nil, err
		}
	}

	return &ScriptShellTool{
		shellTools: shellTools,
		env:        env,
	}, nil
}

func validateConfig(toolName string, tool latest.ScriptShellToolConfig) error {
	// If no required array was set, all arguments are required
	if tool.Required == nil {
		tool.Required = make([]string, 0, len(tool.Args))
		for argName := range tool.Args {
			tool.Required = append(tool.Required, argName)
		}
	}

	// Check for typos in args
	var missingArgs []string
	os.Expand(tool.Cmd, func(varName string) string {
		if _, ok := tool.Args[varName]; !ok {
			missingArgs = append(missingArgs, varName)
		}
		return ""
	})
	if len(missingArgs) > 0 {
		return fmt.Errorf("tool '%s' uses undefined args: %v", toolName, missingArgs)
	}

	// Check that all required args are defined
	for _, reqArg := range tool.Required {
		if _, ok := tool.Args[reqArg]; !ok {
			return fmt.Errorf("tool '%s' has required arg '%s' which is not defined in args", toolName, reqArg)
		}
	}

	return nil
}

func (t *ScriptShellTool) Instructions() string {
	var sb strings.Builder
	sb.WriteString("## Custom Shell Tools\n\n")

	for name, tool := range t.shellTools {
		fmt.Fprintf(&sb, "### %s\n", name)
		if tool.Description != "" {
			fmt.Fprintf(&sb, "%s\n", tool.Description)
		} else {
			fmt.Fprintf(&sb, "Runs: `%s`\n", tool.Cmd)
		}

		for argName, argDef := range tool.Args {
			description := argDef.(map[string]any)["description"].(string)
			required := ""
			if slices.Contains(tool.Required, argName) {
				required = " (required)"
			}
			fmt.Fprintf(&sb, "- `%s`: %s%s\n", argName, description, required)
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

func (t *ScriptShellTool) Tools(context.Context) ([]tools.Tool, error) {
	var toolsList []tools.Tool

	for name, toolConfig := range t.shellTools {
		cfg := toolConfig
		toolName := name

		description := cmp.Or(cfg.Description, "Execute shell command: "+cfg.Cmd)

		inputSchema, err := tools.SchemaToMap(map[string]any{
			"type":       "object",
			"properties": defaultPropertyTypes(cfg.Args, "string"),
			"required":   cfg.Required,
		})
		if err != nil {
			return nil, fmt.Errorf("invalid schema for tool %s: %w", toolName, err)
		}

		toolsList = append(toolsList, tools.Tool{
			Name:         toolName,
			Category:     "shell",
			Description:  description,
			Parameters:   inputSchema,
			OutputSchema: tools.MustSchemaFor[string](),
			Handler: func(ctx context.Context, toolCall tools.ToolCall) (*tools.ToolCallResult, error) {
				return t.execute(ctx, &cfg, toolCall)
			},
		})
	}

	return toolsList, nil
}

func (t *ScriptShellTool) execute(ctx context.Context, toolConfig *latest.ScriptShellToolConfig, toolCall tools.ToolCall) (*tools.ToolCallResult, error) {
	var params map[string]any
	if toolCall.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}

	shell, argsPrefix := shellpath.DetectShell()

	cmd := exec.CommandContext(ctx, shell, append(argsPrefix, toolConfig.Cmd)...)
	cmd.Dir = toolConfig.WorkingDir
	// Per-call clone: appending onto t.env would mutate the shared
	// backing array under concurrent calls. Expand nil to os.Environ()
	// so a nil t.env still inherits the parent env (a non-nil empty
	// slice would strip it).
	base := t.env
	if base == nil {
		base = os.Environ()
	}
	envCopy := make([]string, len(base), len(base)+len(toolConfig.Args))
	copy(envCopy, base)
	for key, value := range params {
		if value == nil {
			continue
		}
		// Only forward arguments declared in the tool's schema. The
		// LLM may hallucinate extra keys (e.g. LD_PRELOAD, PATH);
		// without this filter they would land verbatim in the
		// spawned process's environment.
		if _, declared := toolConfig.Args[key]; !declared {
			continue
		}
		valueStr := fmt.Sprintf("%v", value)
		// A NUL byte mid-string silently truncates env entries at the
		// execve boundary; refuse rather than spawn a process with a
		// surprising env.
		if strings.ContainsRune(valueStr, 0) {
			return tools.ResultError(fmt.Sprintf("argument %q contains a NUL byte", key)), nil
		}
		envCopy = append(envCopy, key+"="+valueStr)
	}
	cmd.Env = envCopy

	output, err := cmd.CombinedOutput()
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Error executing command '%s': %s\nOutput: %s", toolConfig.Cmd, err, limitOutput(string(output)))), nil
	}

	return tools.ResultSuccess(limitOutput(string(output))), nil
}

// defaultPropertyTypes returns a copy of properties where any property
// missing a "type" field gets the given default type.
func defaultPropertyTypes(properties map[string]any, defaultType string) map[string]any {
	result := make(map[string]any, len(properties))
	for k, v := range properties {
		if prop, ok := v.(map[string]any); ok && prop["type"] == nil {
			propCopy := make(map[string]any, len(prop)+1)
			maps.Copy(propCopy, prop)
			propCopy["type"] = defaultType
			result[k] = propCopy
			continue
		}
		result[k] = v
	}
	return result
}
