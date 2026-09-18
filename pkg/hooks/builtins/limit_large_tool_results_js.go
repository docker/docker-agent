//go:build js

package builtins

import (
	"context"
	"fmt"

	"github.com/docker/docker-agent/pkg/hooks"
)

// The browser has no filesystem to spill the full result to, so only the
// tail is kept and the notice says so instead of pointing at a file. The
// filesystem toolset is not available in the browser, so there is no
// read_file head case. session_end has nothing to clean up.
func limitLargeToolResults(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
	if in == nil || in.HookEventName != hooks.EventToolResponseTransform || !largeResultCategories[in.ToolCategory] {
		return nil, nil
	}

	payload, ok := in.ToolResponse.(string)
	if !ok || !largeToolResultLimitExceeded(payload) {
		return nil, nil
	}

	updated := fmt.Sprintf(
		"Tool call result was too large (%d bytes; limit %d bytes). The full result is not available in the browser; narrow the tool query to get the part you need.\n\nShowing the last %d lines (up to %d bytes):\n\n%s",
		len(payload),
		maxToolCallResultBytes,
		largeToolCallResultTailLines,
		largeToolCallResultTailBytes,
		tailLargeToolResult(payload),
	)
	return &hooks.Output{
		HookSpecificOutput: &hooks.HookSpecificOutput{
			HookEventName:       hooks.EventToolResponseTransform,
			UpdatedToolResponse: &updated,
		},
	}, nil
}
