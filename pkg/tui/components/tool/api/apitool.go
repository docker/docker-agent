package api

import (
	"encoding/json"
	"fmt"

	"github.com/docker/go-units"

	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func New(msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	return toolcommon.NewBase(msg, sessionState, render)
}

func render(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, _ int) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(msg.ToolCall.Function.Arguments), &args); err != nil {
		return toolcommon.RenderTool(msg, s, "", "", width, sessionState.HideToolResults())
	}

	// Extract argument summary for the tool call display
	var params string
	if argsText := formatArgs(args); argsText != "" {
		params = "(" + argsText + ")"
	}

	// Add inline result/progress after the tool name
	switch msg.ToolStatus {
	case types.ToolStatusRunning:
		// While running, show what we're calling
		if endpoint := extractEndpoint(args); endpoint != "" {
			params += styles.MutedStyle.Render(": Calling " + endpoint)
		}
	case types.ToolStatusCompleted:
		// When completed, show a brief summary inline
		params += styles.MutedStyle.Render(": Received " + units.HumanSize(float64(len(msg.Content))))
	}

	return toolcommon.RenderTool(msg, s, params, "", width, sessionState.HideToolResults())
}

// extractEndpoint tries to find the endpoint/URL being called.
func extractEndpoint(args map[string]any) string {
	if endpoint, ok := args["endpoint"].(string); ok {
		return endpoint
	}
	if url, ok := args["url"].(string); ok {
		return url
	}
	return ""
}

// formatArgs creates a concise string representation of the arguments.
func formatArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}

	// Check for URL or URLs field (common in fetch tools)
	if urlVal, ok := args["url"].(string); ok && urlVal != "" {
		return urlVal
	}
	if urlsVal, ok := args["urls"].([]any); ok && len(urlsVal) > 0 {
		// Extract just the URLs from the array
		var urls []string
		for _, u := range urlsVal {
			if urlStr, ok := u.(string); ok {
				urls = append(urls, urlStr)
			}
		}
		if len(urls) == 1 {
			return urls[0]
		} else if len(urls) > 1 {
			return fmt.Sprintf("%s (+%d more)", urls[0], len(urls)-1)
		}
	}

	// Try to find common parameter names that might indicate what's being queried
	for _, key := range []string{"query", "q", "search", "message", "prompt", "text"} {
		if val, ok := args[key]; ok {
			if str, ok := val.(string); ok && str != "" {
				return str
			}
		}
	}

	// Fallback: show JSON
	b, _ := json.Marshal(args)
	return string(b)
}
