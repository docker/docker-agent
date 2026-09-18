package shell

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/shellpath"
	"github.com/docker/docker-agent/pkg/tools"
)

// CreateScriptToolSet is used by the tools registry.
func CreateScriptToolSet(ctx context.Context, toolset latest.Toolset, runConfig *config.RuntimeConfig) (tools.ToolSet, error) {
	if len(toolset.Shell) == 0 {
		return nil, errors.New("shell is required for script toolset")
	}

	env, err := environment.ExpandAll(ctx, environment.ToValues(toolset.Env), runConfig.EnvProvider())
	if err != nil {
		return nil, fmt.Errorf("failed to expand the tool's environment variables: %w", err)
	}
	// Prepend os.Environ() so spawned processes inherit the host environment
	// while the configured toolset env still wins on key collisions
	// (exec.Cmd dedupes with last-wins).
	env = append(os.Environ(), env...)
	return NewScript(toolset.Shell, env)
}

type ScriptToolSet struct {
	shellTools map[string]latest.ScriptShellToolConfig
	env        []string
}

// Verify interface compliance
var (
	_ tools.ToolSet      = (*ScriptToolSet)(nil)
	_ tools.Instructable = (*ScriptToolSet)(nil)
)

func NewScript(shellTools map[string]latest.ScriptShellToolConfig, env []string) (*ScriptToolSet, error) {
	for _, e := range env {
		if strings.ContainsRune(e, 0) {
			return nil, errors.New("toolset environment contains a NUL byte")
		}
	}

	for toolName, tool := range shellTools {
		if err := validateConfig(toolName, tool); err != nil {
			return nil, err
		}
	}

	return &ScriptToolSet{
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

	// Check for typos in args. Keys of the per-tool env are legitimate
	// references too, since they are set on the spawned process.
	var missingArgs []string
	var jsEnvRefs []string
	os.Expand(tool.Cmd, func(varName string) string {
		if _, ok := tool.Args[varName]; ok {
			return ""
		}
		if _, ok := tool.Env[varName]; ok {
			return ""
		}
		// `${env.X}` in cmd looks like the config-wide JS-template syntax
		// but cmd is a shell script: report it separately with a fix.
		if strings.HasPrefix(varName, "env.") {
			jsEnvRefs = append(jsEnvRefs, varName)
			return ""
		}
		missingArgs = append(missingArgs, varName)
		return ""
	})
	if len(jsEnvRefs) > 0 {
		return fmt.Errorf("tool '%s' uses ${%s} in cmd; cmd is a shell script, so declare the variable under the tool's env (e.g. env: {%s: \"${%s}\"}) and reference it as $%s",
			toolName, jsEnvRefs[0], strings.TrimPrefix(jsEnvRefs[0], "env."), jsEnvRefs[0], strings.TrimPrefix(jsEnvRefs[0], "env."))
	}
	if len(missingArgs) > 0 {
		return fmt.Errorf("tool '%s' uses undefined args: %v", toolName, missingArgs)
	}

	// An arg and an env entry with the same key would race on the spawned
	// process's environment (args win by exec.Cmd last-wins dedup); reject
	// the ambiguity instead.
	for key := range tool.Env {
		if _, ok := tool.Args[key]; ok {
			return fmt.Errorf("tool '%s' declares '%s' both in args and env; rename one", toolName, key)
		}
	}

	// Check that all required args are defined
	for _, reqArg := range tool.Required {
		if _, ok := tool.Args[reqArg]; !ok {
			return fmt.Errorf("tool '%s' has required arg '%s' which is not defined in args", toolName, reqArg)
		}
	}

	return nil
}

func (t *ScriptToolSet) Instructions() string {
	var sb strings.Builder
	sb.WriteString("## Custom Shell Tools\n\n")

	names := make([]string, 0, len(t.shellTools))
	for name := range t.shellTools {
		names = append(names, name)
	}
	slices.Sort(names)

	for _, name := range names {
		tool := t.shellTools[name]
		fmt.Fprintf(&sb, "### %s\n", name)
		if tool.Description != "" {
			fmt.Fprintf(&sb, "%s\n", tool.Description)
		} else {
			fmt.Fprintf(&sb, "Runs: `%s`\n", tool.Cmd)
		}

		argNames := make([]string, 0, len(tool.Args))
		for argName := range tool.Args {
			argNames = append(argNames, argName)
		}
		slices.Sort(argNames)
		for _, argName := range argNames {
			argDef := tool.Args[argName]
			description := ""
			if m, ok := argDef.(map[string]any); ok {
				if d, ok := m["description"].(string); ok {
					description = d
				}
			}
			required := ""
			if slices.Contains(tool.Required, argName) {
				required = " (required)"
			}
			if description != "" {
				fmt.Fprintf(&sb, "- `%s`: %s%s\n", argName, description, required)
			} else {
				fmt.Fprintf(&sb, "- `%s`%s\n", argName, required)
			}
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

func (t *ScriptToolSet) Tools(context.Context) ([]tools.Tool, error) {
	var toolsList []tools.Tool

	names := make([]string, 0, len(t.shellTools))
	for name := range t.shellTools {
		names = append(names, name)
	}
	slices.Sort(names)

	for _, name := range names {
		cfg := t.shellTools[name]

		description := cmp.Or(cfg.Description, "Execute shell command: "+cfg.Cmd)

		inputSchema, err := tools.SchemaToMap(map[string]any{
			"type":       "object",
			"properties": defaultPropertyTypes(cfg.Args, "string"),
			"required":   cfg.Required,
		})
		if err != nil {
			return nil, fmt.Errorf("invalid schema for tool %s: %w", name, err)
		}

		toolsList = append(toolsList, tools.Tool{
			Name:         name,
			Category:     "shell",
			Description:  description,
			Parameters:   inputSchema,
			OutputSchema: tools.MustSchemaFor[string](),
			Handler: func(ctx context.Context, toolCall tools.ToolCall, rt tools.Runtime) (*tools.ToolCallResult, error) {
				return t.execute(ctx, rt, &cfg, toolCall)
			},
		})
	}

	return toolsList, nil
}

func (t *ScriptToolSet) execute(ctx context.Context, rt tools.Runtime, toolConfig *latest.ScriptShellToolConfig, toolCall tools.ToolCall) (*tools.ToolCallResult, error) {
	var params map[string]any
	if toolCall.Function.Arguments != "" {
		if err := tools.UnmarshalToolArguments(ctx, toolCall, &params); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}

	// working_dir accepts ~, $VAR, ${VAR} and ${env.VAR} like every other
	// working_dir field (issue #2615).
	workingDir := path.ExpandWorkingDir("script shell working_dir", toolConfig.WorkingDir)

	// Stamp the script_shell call shape onto the active span. Cmd
	// ships unconditionally for the same reason as shell.RunShell —
	// see that comment for the redact-at-collector guidance.
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(
			attribute.String("cagent.tool.script_shell.tool_name", toolCall.Function.Name),
			attribute.String("cagent.tool.script_shell.cmd", toolConfig.Cmd),
			attribute.String("cagent.tool.script_shell.cwd", cmp.Or(workingDir, ".")),
		)
	}

	shell, argsPrefix := shellpath.DetectShell()

	cmd := exec.CommandContext(ctx, shell, append(argsPrefix, toolConfig.Cmd)...)
	cmd.Dir = workingDir
	// Per-call clone: appending onto t.env would mutate the shared
	// backing array under concurrent calls. Expand nil to os.Environ()
	// so a nil t.env still inherits the parent env (a non-nil empty
	// slice would strip it).
	base := t.env
	if base == nil {
		base = os.Environ()
	}
	envCopy := make([]string, len(base), len(base)+len(toolConfig.Env)+len(toolConfig.Args))
	copy(envCopy, base)
	// Per-tool env overrides the toolset-level env (exec.Cmd dedupes with
	// last-wins). Only the plain ${env.X} form is expanded; $X and ${X}
	// stay literal because env values may legitimately contain $ (issue
	// #2615).
	for _, key := range slices.Sorted(maps.Keys(toolConfig.Env)) {
		val := path.ExpandEnvRefs(toolConfig.Env[key])
		if strings.ContainsRune(val, 0) {
			return tools.ResultError(fmt.Sprintf("configured environment variable %q contains a NUL byte", key)), nil
		}
		envCopy = append(envCopy, key+"="+val)
	}
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
		// Go's os/exec rejects NUL bytes with generic errors. We check here
		// to provide a clearer error message.
		if strings.ContainsRune(valueStr, 0) {
			return tools.ResultError(fmt.Sprintf("argument %q contains a NUL byte", key)), nil
		}
		envCopy = append(envCopy, key+"="+valueStr)
	}
	cmd.Env = envCopy

	output := newCommandOutput(ctx, rt)
	cmd.Stdout = output
	cmd.Stderr = output

	err := cmd.Run()
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Error executing command '%s': %s\nOutput: %s", toolConfig.Cmd, err, output.String())), nil
	}

	return tools.ResultSuccess(output.String()), nil
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
