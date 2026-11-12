package acp

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

	"github.com/coder/acp-go-sdk"

	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
)

// buildToolCallStart creates a tool call start update.
func buildToolCallStart(toolCall tools.ToolCall, tool tools.Tool, workingDir string) acp.SessionUpdate {
	kind := determineToolKind(toolCall.Function.Name, tool)
	args := parseToolCallArguments(toolCall.Function.Arguments)
	title := toolCallTitle(toolCall.Function.Name, tool.Annotations.Title, args)
	locations := extractLocations(args, workingDir)

	opts := []acp.ToolCallStartOpt{
		func(call *acp.SessionUpdateToolCall) { call.Meta = toolNameMeta(toolCall.Function.Name) },
		acp.WithStartKind(kind),
		acp.WithStartStatus(acp.ToolCallStatusInProgress),
		acp.WithStartRawInput(args),
	}

	if len(locations) > 0 {
		// WithStartLocations also injects rawInput.path, changing non-path arguments.
		opts = append(opts, func(call *acp.SessionUpdateToolCall) { call.Locations = locations })
	}

	return acp.StartToolCall(
		acp.ToolCallId(toolCall.ID),
		title,
		opts...,
	)
}

// buildToolCallComplete creates a tool call completion update.
func buildToolCallComplete(event *runtime.ToolCallResponseEvent) acp.SessionUpdate {
	status := acp.ToolCallStatusCompleted
	if event.Result != nil && event.Result.IsError {
		status = acp.ToolCallStatusFailed
	}
	content := []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(event.Response))}
	if status == acp.ToolCallStatusCompleted && event.Result != nil {
		if change, ok := event.Result.Meta.(*fileChange); ok && change != nil && filepath.IsAbs(change.path) {
			content = append(content, acp.ToolCallContent{Diff: &acp.ToolCallContentDiff{
				Type: "diff", Path: change.path, OldText: change.oldText, NewText: change.newText,
			}})
		}
	}
	return acp.UpdateToolCall(
		acp.ToolCallId(event.ToolCallID),
		func(call *acp.SessionToolCallUpdate) { call.Meta = toolNameMeta(event.ToolDefinition.Name) },
		acp.WithUpdateStatus(status),
		acp.WithUpdateContent(content),
		acp.WithUpdateRawOutput(map[string]any{"content": event.Response}),
	)
}

// buildToolCallUpdate creates a tool call update for permission requests.
func buildToolCallUpdate(toolCall tools.ToolCall, tool tools.Tool, status acp.ToolCallStatus, workingDir string) acp.ToolCallUpdate {
	kind := determineToolKind(toolCall.Function.Name, tool)
	args := parseToolCallArguments(toolCall.Function.Arguments)
	title := toolCallTitle(toolCall.Function.Name, tool.Annotations.Title, args)
	return acp.ToolCallUpdate{
		Meta:       toolNameMeta(toolCall.Function.Name),
		ToolCallId: acp.ToolCallId(toolCall.ID),
		Title:      &title,
		Kind:       &kind,
		Status:     &status,
		RawInput:   args,
		Locations:  extractLocations(args, workingDir),
	}
}

// toolCallTitle appends the command or path so an approval prompt is not just "Shell".
func toolCallTitle(toolName, annotation string, args map[string]any) string {
	base := cmp.Or(annotation, toolName)
	detail := toolCallDetail(toolName, args)
	if detail == "" {
		return base
	}
	return base + ": " + detail
}

func toolCallDetail(toolName string, args map[string]any) string {
	var keys []string
	switch {
	case toolName == "shell" || strings.HasPrefix(toolName, "run_") ||
		strings.HasPrefix(toolName, "exec_") || strings.HasPrefix(toolName, "stop_"):
		keys = []string{"cmd", "command", "script"}
	case strings.HasPrefix(toolName, "read_") || strings.HasPrefix(toolName, "write_") || strings.HasPrefix(toolName, "edit_") ||
		strings.HasPrefix(toolName, "delete_") || strings.HasPrefix(toolName, "create_") || toolName == "directory_tree":
		keys = []string{"path", "file", "filepath", "file_path", "filename"}
	case toolName == "fetch" || strings.HasPrefix(toolName, "http_"):
		keys = []string{"url"}
	default:
		return ""
	}
	for _, key := range keys {
		value, ok := args[key].(string)
		if !ok {
			continue
		}
		value = strings.Join(strings.Fields(value), " ")
		if value == "" {
			continue
		}
		const maxRunes = 80
		runes := []rune(value)
		if len(runes) > maxRunes {
			value = string(runes[:maxRunes-1]) + "…"
		}
		return value
	}
	return ""
}

// determineToolKind maps tool names and annotations to ACP tool kinds.
func determineToolKind(toolName string, tool tools.Tool) acp.ToolKind {
	switch {
	case strings.HasPrefix(toolName, "read_"),
		strings.HasPrefix(toolName, "get_"),
		strings.HasPrefix(toolName, "list_"),
		toolName == "directory_tree":
		return acp.ToolKindRead

	case strings.HasPrefix(toolName, "edit_"),
		strings.HasPrefix(toolName, "write_"),
		strings.HasPrefix(toolName, "update_"),
		strings.HasPrefix(toolName, "create_"),
		strings.HasPrefix(toolName, "add_"):
		return acp.ToolKindEdit

	case strings.HasPrefix(toolName, "delete_"),
		strings.HasPrefix(toolName, "remove_"):
		return acp.ToolKindDelete

	case strings.HasPrefix(toolName, "search_"),
		strings.HasPrefix(toolName, "find_"):
		return acp.ToolKindSearch

	case toolName == "think":
		return acp.ToolKindThink

	case toolName == "fetch",
		strings.HasPrefix(toolName, "http_"):
		return acp.ToolKindFetch

	case toolName == "shell",
		strings.HasPrefix(toolName, "stop_"),
		strings.HasPrefix(toolName, "run_"),
		strings.HasPrefix(toolName, "exec_"):
		return acp.ToolKindExecute

	case toolName == "transfer_task",
		toolName == "handoff":
		return acp.ToolKindSwitchMode

	default:
		if tool.Annotations.ReadOnlyHint {
			return acp.ToolKindRead
		}
		return acp.ToolKindOther
	}
}

// extractLocations extracts file locations from tool call arguments.
func extractLocations(args map[string]any, workingDir string) []acp.ToolCallLocation {
	var locations []acp.ToolCallLocation

	pathKeys := []string{"path", "file", "filepath", "filename", "file_path"}
	for _, key := range pathKeys {
		pathVal, ok := args[key].(string)
		if !ok || pathVal == "" {
			continue
		}
		path := toolLocationPath(pathVal, workingDir)
		if path == "" {
			break
		}
		loc := acp.ToolCallLocation{Path: path}
		if line, ok := args["line"].(float64); ok && line >= 1 && line < float64(int(^uint(0)>>1)) && line == math.Trunc(line) {
			lineInt := int(line)
			loc.Line = &lineInt
		}
		locations = append(locations, loc)
		break
	}

	if paths, ok := args["paths"].([]any); ok {
		for _, p := range paths {
			pathStr, ok := p.(string)
			if !ok {
				continue
			}
			if path := toolLocationPath(pathStr, workingDir); path != "" {
				locations = append(locations, acp.ToolCallLocation{Path: path})
			}
		}
	}

	return locations
}

// toolLocationPath normalizes display metadata without accessing the filesystem.
func toolLocationPath(path, workingDir string) string {
	if path == "" || strings.ContainsRune(path, 0) {
		return ""
	}
	path, err := pathx.ExpandHomeDir(path)
	if err != nil {
		return ""
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	if filepath.VolumeName(path) != "" || (filepath.Separator == '\\' && strings.ContainsAny(path[:1], `/\`)) {
		return ""
	}
	if uri, err := url.Parse(path); err == nil && uri.Scheme != "" {
		return ""
	}
	if !filepath.IsAbs(workingDir) {
		return ""
	}
	return filepath.Clean(filepath.Join(workingDir, path))
}

func parseToolCallArguments(argsJSON string) map[string]any {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		slog.Warn("Failed to parse tool call arguments", "error", err)
		return map[string]any{"raw": argsJSON}
	}
	return args
}

func isTodoTool(toolName string) bool {
	return slices.Contains([]string{
		todo.ToolNameCreateTodo,
		todo.ToolNameCreateTodos,
		todo.ToolNameUpdateTodos,
		todo.ToolNameListTodos,
	}, toolName)
}

// buildPlanUpdateFromTodos converts todo metadata to an ACP plan update.
func buildPlanUpdateFromTodos(meta any) *acp.SessionUpdate {
	todos, ok := meta.([]todo.Todo)
	if !ok {
		slog.Debug("Todo meta is not []todo.Todo", "type", fmt.Sprintf("%T", meta))
		return nil
	}

	entries := make([]acp.PlanEntry, 0, len(todos))
	for _, td := range todos {
		entries = append(entries, acp.PlanEntry{
			Content:  td.Description,
			Status:   mapTodoStatusToACP(td.Status),
			Priority: acp.PlanEntryPriorityMedium,
		})
	}

	return new(acp.UpdatePlan(entries...))
}

func mapTodoStatusToACP(status string) acp.PlanEntryStatus {
	switch status {
	case "pending":
		return acp.PlanEntryStatusPending
	case "in-progress":
		return acp.PlanEntryStatusInProgress
	case "completed":
		return acp.PlanEntryStatusCompleted
	default:
		return acp.PlanEntryStatusPending
	}
}
