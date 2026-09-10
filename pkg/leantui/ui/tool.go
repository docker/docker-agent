package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	toolcomponent "github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	tuitypes "github.com/docker/docker-agent/pkg/tui/types"
)

// ToolView is the render state of a single tool call. It deliberately stores
// the same TUI message shape used by the full-screen TUI so the lean renderer
// can delegate the visual representation to pkg/tui/components/tool.
type ToolView struct {
	message   *tuitypes.Message
	images    []InlineImage
	lastWidth int
	lastLines []string
}

func (t *ToolView) Message() *tuitypes.Message {
	if t == nil {
		return nil
	}
	return t.message
}

func (t *ToolView) SetImages(images []InlineImage) {
	if t != nil {
		t.images = images
	}
}

const MaxToolOutputLines = 12

// NewToolView creates a tool call render model.
func NewToolView(agentName string, toolCall tools.ToolCall, toolDef tools.Tool, status tuitypes.ToolStatus) *ToolView {
	return &ToolView{
		message: tuitypes.ToolCallMessage(agentName, toolCall, EnsureToolDefinition(toolCall, toolDef), status),
	}
}

// EnsureToolDefinition fills a missing tool definition name from the call.
func EnsureToolDefinition(toolCall tools.ToolCall, toolDef tools.Tool) tools.Tool {
	if toolDef.Name == "" {
		toolDef.Name = toolCall.Function.Name
	}
	return toolDef
}

// RenderTool renders a tool call with the same renderer registry used by the
// full TUI. This keeps built-in tools and registered custom renderers visually
// consistent between the normal and lean interfaces.
func RenderTool(t ToolView, width int) []string {
	return RenderToolWithState(&t, width, 0, service.StaticSessionState{})
}

// RenderToolWithState renders a tool call using session state.
func RenderToolWithState(t *ToolView, width, frame int, sessionState service.SessionStateReader) []string {
	if width < 1 {
		width = 1
	}
	if t == nil || t.message == nil {
		return nil
	}
	if sessionState == nil {
		sessionState = service.StaticSessionState{}
	}

	boxStyle := StToolBox(width)
	innerWidth := max(width-boxStyle.GetHorizontalFrameSize(), 1)

	ar := animation.NewSnapshotRuntime(time.Duration(frame) * animation.ChatSpinnerFrameDuration)
	view := toolcomponent.New(ar, t.message, sessionState)
	view.SetSize(innerWidth, 0)
	if t.message.ToolStatus == tuitypes.ToolStatusPending || t.message.ToolStatus == tuitypes.ToolStatusRunning {
		defer animation.StopView(view)
	}

	lines := splitRenderedLines(renderToolBox(view.View(), width), width)
	for _, img := range t.images {
		lines = append(lines, RenderInlineImage(img, width)...)
	}

	if t.shouldKeepLastPendingLines(width, lines) {
		return cloneLines(t.lastLines)
	}
	if t.message.ToolStatus == tuitypes.ToolStatusPending && len(lines) > 0 {
		t.lastWidth = width
		t.lastLines = cloneLines(lines)
	} else if t.message.ToolStatus != tuitypes.ToolStatusPending {
		t.lastLines = nil
		t.lastWidth = 0
	}
	return lines
}

func renderToolBox(content string, width int) string {
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return ""
	}
	return styles.RenderComposite(StToolBox(width), content)
}

func (t *ToolView) shouldKeepLastPendingLines(width int, lines []string) bool {
	if t.message.ToolStatus != tuitypes.ToolStatusPending || t.lastWidth != width || len(t.lastLines) == 0 {
		return false
	}
	if len(lines) == 0 || len(lines) < len(t.lastLines) {
		return true
	}
	return len(lines) == len(t.lastLines) && totalContentWidth(lines) < totalContentWidth(t.lastLines)
}

func cloneLines(lines []string) []string {
	return append([]string(nil), lines...)
}

func totalContentWidth(lines []string) int {
	total := 0
	for _, line := range lines {
		total += DisplayWidth(strings.TrimRight(ansi.Strip(line), " "))
	}
	return total
}

func splitRenderedLines(rendered string, width int) []string {
	if width < 1 {
		width = 1
	}
	rendered = strings.TrimRight(rendered, "\n")
	if rendered == "" {
		return nil
	}

	var out []string
	for line := range strings.SplitSeq(rendered, "\n") {
		if DisplayWidth(line) > width {
			out = append(out, WrapANSI(line, width)...)
			continue
		}
		out = append(out, line)
	}
	return out
}

// RenderToolOutput renders plain streamed shell output.
func RenderToolOutput(output string, width int) []string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")

	var out []string
	if len(lines) > MaxToolOutputLines {
		hidden := len(lines) - MaxToolOutputLines
		out = append(out, "  "+StMuted().Render(fmt.Sprintf("… (%d earlier lines)", hidden)))
		lines = lines[len(lines)-MaxToolOutputLines:]
	}
	for _, l := range lines {
		for _, wl := range WrapANSI(l, width-2) {
			out = append(out, "  "+StMuted().Render(wl))
		}
	}
	return out
}
