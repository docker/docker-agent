package acp

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
)

// buildToolCallStart creates a tool call start update.
func buildToolCallStart(toolCall tools.ToolCall, tool tools.Tool) acp.SessionUpdate {
	kind := determineToolKind(toolCall.Function.Name, tool)
	title := cmp.Or(tool.Annotations.Title, toolCall.Function.Name)

	args := parseToolCallArguments(toolCall.Function.Arguments)
	locations := extractLocations(args)

	opts := []acp.ToolCallStartOpt{
		acp.WithStartKind(kind),
		acp.WithStartStatus(acp.ToolCallStatusPending),
		acp.WithStartRawInput(args),
	}

	if len(locations) > 0 {
		opts = append(opts, acp.WithStartLocations(locations))
	}

	return acp.StartToolCall(
		acp.ToolCallId(toolCall.ID),
		title,
		opts...,
	)
}

// buildToolCallComplete creates a tool call completion update.
func buildToolCallComplete(arguments string, event *runtime.ToolCallResponseEvent) acp.SessionUpdate {
	status := acp.ToolCallStatusCompleted
	if event.Result != nil && event.Result.IsError {
		status = acp.ToolCallStatusFailed
	}

	if isFileEditTool(event.ToolDefinition.Name) {
		if diffContent := extractDiffContent(event.ToolDefinition.Name, arguments); diffContent != nil {
			return acp.UpdateToolCall(
				acp.ToolCallId(event.ToolCallID),
				acp.WithUpdateStatus(status),
				acp.WithUpdateContent([]acp.ToolCallContent{*diffContent}),
				acp.WithUpdateRawOutput(map[string]any{"content": event.Response}),
			)
		}
	}

	return acp.UpdateToolCall(
		acp.ToolCallId(event.ToolCallID),
		acp.WithUpdateStatus(status),
		acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.TextBlock(event.Response))}),
		acp.WithUpdateRawOutput(map[string]any{"content": event.Response}),
	)
}

// buildToolCallUpdate creates a tool call update for permission requests.
func buildToolCallUpdate(toolCall tools.ToolCall, tool tools.Tool, status acp.ToolCallStatus) acp.ToolCallUpdate {
	kind := acp.ToolKindExecute
	title := cmp.Or(tool.Annotations.Title, toolCall.Function.Name)

	if tool.Annotations.ReadOnlyHint {
		kind = acp.ToolKindRead
	}

	return acp.ToolCallUpdate{
		ToolCallId: acp.ToolCallId(toolCall.ID),
		Title:      &title,
		Kind:       &kind,
		Status:     &status,
		RawInput:   parseToolCallArguments(toolCall.Function.Arguments),
	}
}

// determineToolKind maps tool names and annotations to ACP tool kinds.
func determineToolKind(toolName string, tool tools.Tool) acp.ToolKind {
	if tool.Annotations.ReadOnlyHint {
		return acp.ToolKindRead
	}
	if tool.Annotations.DestructiveHint != nil && *tool.Annotations.DestructiveHint {
		return acp.ToolKindDelete
	}

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
		strings.HasPrefix(toolName, "remove_"),
		strings.HasPrefix(toolName, "stop_"):
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
		strings.HasPrefix(toolName, "run_"),
		strings.HasPrefix(toolName, "exec_"):
		return acp.ToolKindExecute

	case toolName == "transfer_task",
		toolName == "handoff":
		return acp.ToolKindSwitchMode

	default:
		return acp.ToolKindOther
	}
}

// extractLocations extracts file locations from tool call arguments.
func extractLocations(args map[string]any) []acp.ToolCallLocation {
	var locations []acp.ToolCallLocation

	pathKeys := []string{"path", "file", "filepath", "filename", "file_path"}
	for _, key := range pathKeys {
		if pathVal, ok := args[key].(string); ok && pathVal != "" {
			loc := acp.ToolCallLocation{Path: pathVal}
			if line, ok := args["line"].(float64); ok {
				lineInt := int(line)
				loc.Line = &lineInt
			}
			locations = append(locations, loc)
			break
		}
	}

	if paths, ok := args["paths"].([]any); ok {
		for _, p := range paths {
			if pathStr, ok := p.(string); ok && pathStr != "" {
				locations = append(locations, acp.ToolCallLocation{Path: pathStr})
			}
		}
	}

	return locations
}

func isFileEditTool(toolName string) bool {
	return slices.Contains([]string{"edit_file", "write_file"}, toolName)
}

// extractDiffContent tries to create a diff content block from edit tool arguments.
func extractDiffContent(toolCallName, arguments string) *acp.ToolCallContent {
	args := parseToolCallArguments(arguments)

	path, ok := args["path"].(string)
	if !ok || path == "" {
		return nil
	}

	if toolCallName == "edit_file" {
		edits, ok := args["edits"].([]any)
		if !ok || len(edits) == 0 {
			return nil
		}

		var oldTextSb, newTextSb strings.Builder
		for _, edit := range edits {
			editMap, ok := edit.(map[string]any)
			if !ok {
				continue
			}
			if old, ok := editMap["oldText"].(string); ok {
				oldTextSb.WriteString(old)
				oldTextSb.WriteByte('\n')
			}
			if newVal, ok := editMap["newText"].(string); ok {
				newTextSb.WriteString(newVal)
				newTextSb.WriteByte('\n')
			}
		}
		oldText := oldTextSb.String()
		newText := newTextSb.String()

		if oldText != "" || newText != "" {
			diff := acp.ToolDiffContent(path, newText, oldText)
			return &diff
		}
	}

	if toolCallName == "write_file" {
		if content, ok := args["content"].(string); ok {
			diff := acp.ToolDiffContent(path, content)
			return &diff
		}
	}

	return nil
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

	if len(todos) == 0 {
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

	update := acp.UpdatePlan(entries...)
	return &update
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
