package builtins

import (
	"context"
	"fmt"
	"strings"
	"text/template"

	"github.com/docker/docker-agent/pkg/hooks"
)

// AddContext is the registered name of the add_context builtin.
const AddContext = "add_context"

func addContext(_ context.Context, in *hooks.Input, args []string) (*hooks.Output, error) {
	if in == nil || len(args) == 0 {
		return nil, nil
	}

	var contents []string
	for i, arg := range args {
		tpl, err := template.New(AddContext).Option("missingkey=error").Parse(arg)
		if err != nil {
			return nil, fmt.Errorf("add_context: parse arg %d: %w", i+1, err)
		}
		var buf strings.Builder
		if err := tpl.Execute(&buf, in); err != nil {
			return nil, fmt.Errorf("add_context: render arg %d: %w", i+1, err)
		}
		if content := buf.String(); strings.TrimSpace(content) != "" {
			contents = append(contents, content)
		}
	}
	return hooks.NewAdditionalContextOutput(in.HookEventName, strings.Join(contents, "\n")), nil
}
