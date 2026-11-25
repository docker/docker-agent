package permissions

import (
	"context"
	"strings"

	"github.com/docker/docker-agent/pkg/safety"
	"github.com/docker/docker-agent/pkg/tools"
)

// BuildPermissionPattern creates the permission pattern granted by the
// "always allow" decision. For command tools (shell, run_background_job)
// it extracts the first word of the command and creates a pattern like
// "shell:cmd=ls*" matching all invocations of that command; for other
// tools it returns the tool name.
func BuildPermissionPattern(ctx context.Context, toolCall tools.ToolCall) string {
	toolName := toolCall.Function.Name

	if safety.IsCommandTool(toolName) {
		var args map[string]any
		if err := tools.UnmarshalToolArguments(ctx, toolCall, &args); err == nil {
			// Resolve cmd/command the way the handler will, so the grant
			// is built from the command that actually runs. First word
			// only ("ls -la /tmp" -> "ls"); the trailing * matches any
			// arguments.
			cmd, _ := safety.CommandArg(args)
			for field := range strings.FieldsSeq(cmd) {
				return toolName + ":cmd=" + field + "*"
			}
		}
	}

	return toolName
}

// AlwaysAllowLabel is the descriptive label of the "always allow" option
// for a pattern from BuildPermissionPattern: the command pattern for shell
// ("always allow ls*"), the tool name otherwise. Whitespace runs are
// collapsed: a newline would split the label's help row across physical
// lines, breaking the one-line-per-row contract click hit-testing relies
// on, and an interior double space would read like the separator between
// options.
func AlwaysAllowLabel(pattern string) string {
	return "always allow " + PatternLabel(pattern)
}

// PatternLabel is the display form of a pattern from BuildPermissionPattern:
// the command pattern for shell ("ls*"), the tool name otherwise.
func PatternLabel(pattern string) string {
	if _, cmdPattern, ok := strings.Cut(pattern, ":cmd="); ok {
		pattern = cmdPattern
	}
	return strings.Join(strings.Fields(pattern), " ")
}
