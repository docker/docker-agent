package builtins

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/docker/docker-agent/pkg/hooks"
)

// LimitLargeToolResults is the registered name of the builtin
// tool_response_transform hook that stores oversized tool results in a per-session
// temp directory and returns a bounded excerpt plus a notice for the
// conversation: the head for the built-in filesystem read_file (whose
// line/limit arguments let the model fetch later ranges), the tail for
// everything else.
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

// readFileToolName mirrors the filesystem toolset's read_file tool name
// without importing the whole toolset package. read_file results are
// truncated head-first because the beginning of a file (front matter,
// imports, config preamble) is usually what the model needs first, and the
// tool's line/limit arguments make the rest reachable. Matched by
// ToolCategory and ToolName together: other filesystem tools
// (read_multiple_files, search_files_content, ...) and mcp/a2a tools that
// merely share the read_file name have no known line/limit contract, so
// they keep tail truncation.
const readFileToolName = "read_file"

func limitLargeToolResults(ctx context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
	if in == nil {
		return nil, nil
	}

	switch in.HookEventName {
	case hooks.EventToolResponseTransform:
		return limitLargeToolResponse(ctx, in)
	case hooks.EventSessionEnd:
		if err := os.RemoveAll(largeToolResultDir(in.SessionID)); err != nil {
			slog.WarnContext(ctx, "Failed to clean up large tool result temp directory", "error", err)
		}
	}
	return nil, nil
}

func limitLargeToolResponse(ctx context.Context, in *hooks.Input) (*hooks.Output, error) {
	payload, ok := in.ToolResponse.(string)
	if !ok || !limitLargeToolResultsWouldRewrite(in.ToolCategory, payload) {
		return nil, nil
	}

	path, err := writeLargeToolResult(in.SessionID, payload)
	if err != nil {
		slog.WarnContext(ctx, "Failed to write large tool call result to temp file", "error", err)
		return nil, nil
	}

	var updated string
	if in.ToolCategory == filesystemToolCategory && in.ToolName == readFileToolName {
		updated = readFileHeadNotice(in.ToolInput, payload, path)
	} else {
		tail := tailLargeToolResult(payload)
		updated = fmt.Sprintf(
			"Tool call result was too large (%d bytes; limit %d bytes). The full result is available in a file: %s\n\nShowing the last %d lines (up to %d bytes):\n\n%s",
			len(payload),
			maxToolCallResultBytes,
			path,
			largeToolCallResultTailLines,
			largeToolCallResultTailBytes,
			tail,
		)
	}

	return &hooks.Output{
		HookSpecificOutput: &hooks.HookSpecificOutput{
			HookEventName:       hooks.EventToolResponseTransform,
			UpdatedToolResponse: &updated,
		},
	}, nil
}

// readFileHeadNotice renders the oversized-result notice for the built-in
// filesystem read_file: the head excerpt plus continuation advice. When the
// head contains at least one complete line, the notice gives the line/limit
// arguments that fetch the next range. A head without a single newline
// means the first line alone exceeds the byte cap: a line-based call cannot
// advance within that line (and rereading the spill file with read_file
// would be capped by this hook again), so the notice states that limitation
// instead of suggesting a call that would loop on the same line.
func readFileHeadNotice(toolInput map[string]any, payload, path string) string {
	head := headLargeToolResult(payload)
	if !strings.Contains(head, "\n") {
		return fmt.Sprintf(
			"Tool call result was too large (%d bytes; limit %d bytes). The full result is available in a file: %s\n\nShowing the first %d bytes. The first line of this result alone exceeds the excerpt limit, so read_file's line-based \"line\"/\"limit\" arguments cannot advance within it, and reading the file above with read_file would be truncated the same way. To read beyond this excerpt, use a tool or command that can read byte ranges (for example a shell command) on that file:\n\n%s",
			len(payload),
			maxToolCallResultBytes,
			path,
			len(head),
			head,
		)
	}
	return fmt.Sprintf(
		"Tool call result was too large (%d bytes; limit %d bytes). The full result is available in a file: %s\n\nShowing the first %d lines (up to %d bytes). To continue reading, call read_file again with the same path plus \"line\": %d (1-based start line) and a \"limit\" (maximum number of lines):\n\n%s",
		len(payload),
		maxToolCallResultBytes,
		path,
		largeToolCallResultTailLines,
		largeToolCallResultTailBytes,
		nextReadFileLine(toolInput, head),
		head,
	)
}

func largeToolResultLimitExceeded(payload string) bool {
	return len(payload) > maxToolCallResultBytes || lineCount(payload) > largeToolCallResultTailLines
}

// limitLargeToolResultsWouldRewrite reports whether limit_large_tool_results
// would replace this response with its truncation notice.
//
// Exists so another tool_response_transform builtin can tell whether its own
// rewrite would survive: ApplyAgentDefaults prepends limit_large_tool_results
// and the executor keeps the first non-nil rewrite in config order, so
// whenever this returns true nothing a user-configured hook produces for the
// same response is ever seen. Both halves matter — the category gate as much
// as the size, since a category this builtin does not cover is never rewritten
// however large it is.
func limitLargeToolResultsWouldRewrite(toolCategory, payload string) bool {
	return largeResultCategories[toolCategory] && largeToolResultLimitExceeded(payload)
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

func writeLargeToolResult(sessionID, payload string) (string, error) {
	dir := largeToolResultDir(sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	file, err := os.CreateTemp(dir, "tool-result-*.txt")
	if err != nil {
		return "", err
	}
	path := file.Name()

	_, writeErr := file.WriteString(payload)
	closeErr := file.Close()
	if writeErr != nil {
		_ = os.Remove(path)
		return "", writeErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return "", closeErr
	}
	return path, nil
}

func largeToolResultDir(sessionID string) string {
	if sessionID == "" {
		sessionID = "unknown"
	}
	return filepath.Join(os.TempDir(), "docker-agent-tool-results", url.PathEscape(sessionID))
}

func tailLargeToolResult(payload string) string {
	tail := lastLines([]byte(payload), largeToolCallResultTailLines)
	if len(tail) > largeToolCallResultTailBytes {
		tail = trimToRuneStart(tail[len(tail)-largeToolCallResultTailBytes:])
	}
	return string(tail)
}

func headLargeToolResult(payload string) string {
	head := firstLines([]byte(payload), largeToolCallResultTailLines)
	if len(head) > largeToolCallResultTailBytes {
		head = trimToRuneEnd(head[:largeToolCallResultTailBytes])
	}
	return string(head)
}

// nextReadFileLine computes the 1-based file line to pass as read_file's
// line argument to continue past the truncated head: the truncated call's
// own start line (1 unless it already used line) plus the number of
// complete lines shown. A byte-capped partial trailing line is deliberately
// re-read in full by the follow-up call. Only meaningful for heads with at
// least one complete line: with none the sum repeats the same start line,
// which readFileHeadNotice reports as un-advanceable instead of suggesting.
func nextReadFileLine(toolInput map[string]any, head string) int {
	start := 1
	// Tool arguments arrive JSON-decoded, so numbers are float64.
	if v, ok := toolInput["line"].(float64); ok && v >= 1 {
		start = int(v)
	}
	return start + strings.Count(head, "\n")
}

func trimToRuneStart(data []byte) []byte {
	for len(data) > 0 && !utf8.RuneStart(data[0]) {
		data = data[1:]
	}
	return data
}

// trimToRuneEnd drops a trailing incomplete UTF-8 sequence produced by a
// byte-boundary cut so the head stays valid UTF-8, mirroring
// trimToRuneStart for tails. Bytes that were already invalid before the cut
// are left alone.
func trimToRuneEnd(data []byte) []byte {
	end := len(data)
	for i := end - 1; i >= 0 && i > end-utf8.UTFMax; i-- {
		if !utf8.RuneStart(data[i]) {
			continue
		}
		if !utf8.FullRune(data[i:end]) {
			return data[:i]
		}
		break
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

// firstLines returns the first limit lines of data, line terminators
// included; data with at most limit lines is returned unchanged.
func firstLines(data []byte, limit int) []byte {
	if limit <= 0 || len(data) == 0 {
		return data
	}

	lines := 0
	for i, b := range data {
		if b != '\n' {
			continue
		}
		lines++
		if lines >= limit {
			return data[:i+1]
		}
	}
	return data
}
