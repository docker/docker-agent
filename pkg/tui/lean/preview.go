package lean

import (
	"encoding/json"
	"fmt"
	"strings"
)

type ToolPreview struct {
	Summary string
	Details []string
}

func formatToolPreview(toolName, rawArgs string, status ToolStatus, result string, expanded bool) ToolPreview {
	args := map[string]any{}
	parsed := false
	if err := json.Unmarshal([]byte(rawArgs), &args); err == nil {
		parsed = true
	}
	details := []string{}
	if !parsed {
		partial := strings.TrimSpace(rawArgs)
		if partial != "" {
			details = append(details, "args")
			details = append(details, previewText(partial, ternaryInt(expanded, 16, 4))...)
		}
		if result != "" {
			if status == ToolError {
				details = append(details, "error")
			} else {
				details = append(details, "result")
			}
			details = append(details, previewText(result, ternaryInt(expanded, 20, 6))...)
		}
		return ToolPreview{Summary: toolName, Details: details}
	}

	summary := toolName
	switch toolName {
	case "read_file":
		path := getString(args["path"])
		if path == "" {
			path = "(unknown path)"
		}
		start := getNumber(args["start_line"])
		end := getNumber(args["end_line"])
		switch {
		case start != nil && end != nil:
			summary = fmt.Sprintf("%s:%d-%d", path, int(*start), int(*end))
		case start != nil:
			summary = fmt.Sprintf("%s:%d", path, int(*start))
		default:
			summary = path
		}
	case "write_file":
		path := orDefault(getString(args["path"]), "(unknown path)")
		content := getString(args["content"])
		summary = fmt.Sprintf("%s (%d lines)", path, countLines(content))
		details = append(details, "write preview")
		details = append(details, previewText(content, ternaryInt(expanded, 18, 6))...)
	case "edit_file":
		path := orDefault(getString(args["path"]), "(unknown path)")
		summary = path
		diff := buildSimpleDiff(getString(args["old_text"]), getString(args["new_text"]))
		if len(diff) > 0 {
			details = append(details, "diff preview")
			limit := ternaryInt(expanded, 20, 8)
			if len(diff) > limit {
				diff = diff[:limit]
			}
			details = append(details, diff...)
		}
	case "run_command", "shell", "run_shell", "run_shell_background", "run_background_job":
		cmd := oneLine(getString(args["command"]))
		if cmd == "" {
			cmd = oneLine(getString(args["cmd"]))
		}
		if cmd == "" {
			cmd = "(empty command)"
		}
		summary = "$ " + cmd
		if cwd := getString(args["cwd"]); cwd != "" {
			details = append(details, "cwd: "+cwd)
		}
		if timeout := getNumber(args["timeout"]); timeout != nil {
			details = append(details, fmt.Sprintf("timeout: %gs", *timeout))
		}
	case "list_directory", "directory_tree":
		path := orDefault(getString(args["path"]), ".")
		suffixes := []string{}
		if depth := getNumber(args["depth"]); depth != nil {
			suffixes = append(suffixes, fmt.Sprintf("depth=%d", int(*depth)))
		}
		if limit := getNumber(args["limit"]); limit != nil {
			suffixes = append(suffixes, fmt.Sprintf("limit=%d", int(*limit)))
		}
		if len(suffixes) > 0 {
			summary = fmt.Sprintf("%s (%s)", path, strings.Join(suffixes, ", "))
		} else {
			summary = path
		}
	case "search", "search_files_content":
		pattern := oneLine(getString(args["pattern"]))
		if pattern == "" {
			pattern = oneLine(getString(args["query"]))
		}
		path := orDefault(getString(args["path"]), ".")
		if pattern == "" {
			pattern = "(empty pattern)"
		}
		summary = fmt.Sprintf("%s in %s", pattern, path)
		if glob := getString(args["glob"]); glob != "" {
			details = append(details, "glob: "+glob)
		}
	case "run_agent", "task", "transfer_task", "handoff":
		agent := orDefault(getString(args["agent"]), orDefault(getString(args["name"]), "child"))
		summary = agent
		if input := getString(args["task"]); input != "" {
			details = append(details, "input")
			details = append(details, previewText(input, ternaryInt(expanded, 12, 4))...)
		}
	}

	if result != "" {
		if status == ToolError {
			details = append(details, "error")
		} else {
			details = append(details, "result")
		}
		details = append(details, previewText(result, ternaryInt(expanded, 20, 6))...)
	}

	return ToolPreview{Summary: summary, Details: details}
}

func previewText(text string, maxLines int) []string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return []string{"(empty)"}
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) <= maxLines {
		return lines
	}
	return append(lines[:maxLines], fmt.Sprintf("… %d more line(s)", len(lines)-maxLines))
}

func buildSimpleDiff(oldText, newText string) []string {
	oldLines := strings.Split(oldText, "\n")
	newLines := strings.Split(newText, "\n")
	maxLines := maxInt(len(oldLines), len(newLines))
	out := make([]string, 0, maxLines*2)
	for i := range maxLines {
		var before, after string
		if i < len(oldLines) {
			before = oldLines[i]
		}
		if i < len(newLines) {
			after = newLines[i]
		}
		if before == after {
			if i < len(oldLines) {
				out = append(out, "  "+before)
			}
			continue
		}
		if i < len(oldLines) {
			out = append(out, "- "+before)
		}
		if i < len(newLines) {
			out = append(out, "+ "+after)
		}
	}
	return out
}

func countLines(s string) int {
	if s == "" {
		return 1
	}
	return strings.Count(s, "\n") + 1
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
func getString(v any) string  { s, _ := v.(string); return s }

func getNumber(v any) *float64 {
	n, ok := v.(float64)
	if !ok {
		return nil
	}
	return &n
}
