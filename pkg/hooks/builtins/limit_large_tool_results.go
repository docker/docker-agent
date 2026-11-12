package builtins

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/docker/docker-agent/pkg/chat"
)

// LimitLargeToolResults is the registered name of the builtin
// tool_response_transform hook that stores oversized tool results in a per-session
// temp directory and returns a bounded excerpt plus a notice for the
// conversation: the head for the built-in filesystem read_file (whose
// line/limit arguments let the model fetch later ranges), the tail for
// everything else. The browser build keeps only the tail: there is no
// filesystem to store the full result in.
const LimitLargeToolResults = "limit_large_tool_results"

const (
	maxToolCallResultBytes       = 50 * 1024
	largeToolCallResultTailLines = 2000
	largeToolCallResultTailBytes = 50 * 1024
)

// largeResultCategories lists the tool categories whose results can be
// large even when their capture buffer is bounded, so they are subject
// to the oversized-result cap. filesystem and shell are the high-output
// built-in toolsets alongside background_jobs; mcp and a2a call external servers that impose no
// per-result limit of their own (unlike the openapi/api toolsets, which
// already truncate their output). Internal toolsets (memory, plan, tasks,
// think, ...) return bounded, structured results and are left untouched.
var largeResultCategories = map[string]bool{
	filesystemToolCategory: true,
	"shell":                true,
	"background_jobs":      true,
	"mcp":                  true,
	"a2a":                  true,
}

// filesystemToolCategory is the category of the built-in filesystem toolset.
const filesystemToolCategory = "filesystem"

func largeToolResultLimitExceeded(payload string) bool {
	return len(payload) > maxToolCallResultBytes || lineCount(payload) > largeToolCallResultTailLines
}

func lineCount(payload string) int {
	if payload == "" {
		return 0
	}
	lines := strings.Count(payload, "\n")
	if !strings.HasSuffix(payload, "\n") {
		lines++
	}
	return lines
}

func tailLargeToolResult(payload string) string {
	tail := lastLines([]byte(payload), largeToolCallResultTailLines)
	if len(tail) > largeToolCallResultTailBytes {
		tail = trimToRuneStart(tail[len(tail)-largeToolCallResultTailBytes:])
	}
	return string(tail)
}

func trimToRuneStart(data []byte) []byte {
	for len(data) > 0 && !utf8.RuneStart(data[0]) {
		data = data[1:]
	}
	return data
}

func lastLines(data []byte, limit int) []byte {
	if limit <= 0 || len(data) == 0 {
		return data
	}

	lines := 0
	for i, b := range slices.Backward(data) {
		if b != '\n' {
			continue
		}
		lines++
		if lines > limit {
			return data[i+1:]
		}
	}
	return data
}

// BoundToolResult is the fail-bounded, filesystem-free backstop for eligible
// results. notice describes where omitted output can (or cannot) be recovered.
func BoundToolResult(category, name, payload, notice string) string {
	if !largeResultCategories[category] || len(payload) <= maxToolCallResultBytes {
		return payload
	}
	prefix := fmt.Sprintf("Tool call result was too large (%d bytes; limit %d bytes). %s\n\n", len(payload), maxToolCallResultBytes, notice)
	if category == filesystemToolCategory && name == "read_file" {
		prefix += "Showing the beginning of the result:\n\n"
		return boundedExcerpt(prefix, payload, true)
	}
	return tailToolResultNotice(category, payload, prefix)
}

func boundedExcerpt(prefix, payload string, head bool) string {
	prefix = chat.TruncateUTF8Bytes(prefix, maxToolCallResultBytes/2)
	budget := maxToolCallResultBytes - len(prefix)
	if head {
		return prefix + chat.TruncateUTF8Bytes(payload, budget)
	}
	if len(payload) > budget {
		payload = string(trimToRuneStart([]byte(payload[len(payload)-budget:])))
	}
	return prefix + payload
}

func tailToolResultNotice(category, payload, prefix string) string {
	if category == "background_jobs" {
		// Preserve status/exit code ahead of the log tail, even with a huge command.
		if header, _, ok := strings.Cut(payload, "--- Output ---\n"); ok {
			for line := range strings.SplitSeq(header, "\n") {
				prefix += chat.TruncateUTF8Bytes(line, 512) + "\n"
				if len(prefix) > maxToolCallResultBytes/4 {
					break
				}
			}
		}
	}
	prefix += fmt.Sprintf("Showing the last %d lines (up to %d bytes, including this notice):\n\n", largeToolCallResultTailLines, maxToolCallResultBytes)
	return boundedExcerpt(prefix, tailLargeToolResult(payload), false)
}

func largeToolResultNotice(payload string) string {
	if len(payload) > maxToolCallResultBytes {
		return fmt.Sprintf("Tool call result was too large (%d bytes; limit %d bytes).", len(payload), maxToolCallResultBytes)
	}
	return fmt.Sprintf("Tool call result was too large (%d lines; limit %d lines).", lineCount(payload), largeToolCallResultTailLines)
}
