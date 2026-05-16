package defaulttool

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type kv struct {
	Key   string
	Value any
}

func renderToolArgs(toolCall tools.ToolCall, shortWidth, width int) string {
	args, err := decodeArguments(toolCall.Function.Arguments)
	if err != nil {
		return ""
	}

	// Filter out the friendly description parameter
	filteredArgs := make([]kv, 0, len(args))
	for _, arg := range args {
		if arg.Key != tools.DescriptionParam {
			filteredArgs = append(filteredArgs, arg)
		}
	}

	if len(filteredArgs) == 0 {
		return ""
	}

	var short strings.Builder
	var md strings.Builder
	for i, arg := range filteredArgs {
		if i > 0 {
			short.WriteString(" ")
			md.WriteString("\n")
		}

		content := formatValue(arg.Value)

		fmt.Fprintf(&short, "%s=%s", arg.Key, content)
		fmt.Fprintf(&md, "%s:\n%s", arg.Key, content)
		if !strings.HasSuffix(content, "\n") {
			md.WriteString("\n")
		}
	}

	if lipgloss.Width(short.String()) <= shortWidth && !strings.Contains(short.String(), "\n") {
		return short.String()
	}

	return "\n" + styles.ToolCallArgs.Width(width).Render(strings.TrimSuffix(md.String(), "\n"))
}

// formatValue formats a value for display.
// Single-element arrays are kept on one line, while larger arrays are indented.
func formatValue(value any) string {
	if v, ok := value.(string); ok {
		return v
	}

	// Special handling for arrays: single-element arrays stay on one line
	if arr, ok := value.([]any); ok && len(arr) == 1 {
		buf, err := json.Marshal(arr)
		if err != nil {
			return fmt.Sprintf("%v", value)
		}
		return string(buf)
	}

	buf, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(buf)
}

// decodeArguments decodes the JSON-encoded arguments string into an ordered slice of key-value pairs.
func decodeArguments(arguments string) ([]kv, error) {
	decoder := json.NewDecoder(strings.NewReader(arguments))

	tok, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, err
	}

	var args []kv

	for decoder.More() {
		tok, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, err
		}

		var val any
		if err := decoder.Decode(&val); err != nil {
			return nil, err
		}

		args = append(args, kv{Key: key, Value: val})
	}
	_, _ = decoder.Token()

	return args, nil
}
