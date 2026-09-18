package builtins

import (
	"slices"
	"strings"
	"unicode/utf8"
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
// arbitrarily large and are not bounded anywhere else, so they are subject
// to the oversized-result cap. filesystem and shell are the high-output
// built-in toolsets; mcp and a2a call external servers that impose no
// per-result limit of their own (unlike the openapi/api toolsets, which
// already truncate their output). Internal toolsets (memory, plan, tasks,
// think, ...) return bounded, structured results and are left untouched.
var largeResultCategories = map[string]bool{
	filesystemToolCategory: true,
	"shell":                true,
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
