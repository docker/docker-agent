package toolcommon

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tools"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// SummarizeActivity renders a run of routine tool calls without arguments or output.
func SummarizeActivity(messages []*types.Message, width int) string {
	if len(messages) == 0 {
		return ""
	}
	names := make([]string, 0)
	counts := make(map[string]int)
	running := 0
	description := ""
	for _, msg := range messages {
		name := msg.ToolDefinition.DisplayName()
		if counts[name] == 0 {
			names = append(names, name)
		}
		counts[name]++
		if msg.ToolStatus == types.ToolStatusRunning || msg.ToolStatus == types.ToolStatusPending {
			running++
			description = tools.ExtractDescription(msg.ToolCall.Function.Arguments)
			if description == "" {
				description = name
			}
		}
	}
	parts := make([]string, 0, len(names))
	for _, name := range names {
		label := name
		if counts[name] > 1 {
			label = fmt.Sprintf("%s ×%d", name, counts[name])
		}
		parts = append(parts, label)
	}
	label := fmt.Sprintf("✓ %d tool calls · %s", len(messages), strings.Join(parts, ", "))
	if len(messages) == 1 {
		label = "✓ " + parts[0]
	}
	if running > 0 {
		label = fmt.Sprintf("● %s · %d running · %d completed", description, running, len(messages)-running)
	}
	label = strings.Join(strings.Fields(ansi.Strip(label)), " ")
	return styles.MutedStyle.Render(ansi.Truncate(label, max(width, 1), "…"))
}

func IsRoutineActivity(msg *types.Message) bool {
	return msg != nil && msg.Type == types.MessageTypeToolCall && msg.ToolCall.Function.Name != filesystem.ToolNameEditFile && msg.ToolStatus != types.ToolStatusError && msg.ToolStatus != types.ToolStatusConfirmation
}

func ActivityHeader(messages []*types.Message, width int, expanded bool) string {
	marker := "[+] "
	if expanded {
		marker = "[-] "
	}
	summary := SummarizeActivity(messages, max(width-4, 1))
	if len(messages) == 0 {
		summary = styles.MutedStyle.Render("Thinking")
	}
	return ansi.Truncate(styles.MutedStyle.Render(marker)+summary, max(width, 1), "…")
}
