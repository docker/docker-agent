package ui

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	seqPromptStart = "\x1b]133;A;redraw=0\x07"
	seqOutputStart = "\x1b]133;C\x07"
)

// RenderUserLines renders a submitted user message as committed scrollback,
// echoing it with the same prompt marker used by the input box.
func RenderUserLines(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	boxStyle := StUserBox(width)
	innerWidth := max(width-boxStyle.GetHorizontalFrameSize(), 1)
	textStyle := lipgloss.NewStyle().Foreground(styles.AgentBadgeFg)
	content := strings.Join(RenderUserLinesWith(text, innerWidth, textStyle.Bold(true), textStyle), "\n")
	lines := splitRenderedLines(styles.RenderComposite(boxStyle, content), width)
	if len(lines) > 0 {
		lines[0] = seqPromptStart + lines[0] + seqOutputStart
	}
	return lines
}

func RenderPendingUserLines(msg PendingUserMessage, width int) []string {
	label := "Steering: "
	if msg.Kind == PendingUserFollowUp {
		label = "Follow-up: "
	}
	muted := StMuted()
	return RenderUserLinesWith(label+msg.Display, width, muted, muted)
}

func RenderUserLinesWith(text string, width int, promptStyle, textStyle lipgloss.Style) []string {
	text = strings.TrimRight(text, "\n")
	wrapped := WrapANSI(text, width-PromptWidth)
	out := make([]string, 0, len(wrapped))
	for i, line := range wrapped {
		prefix := promptStyle.Render(PromptText)
		if i > 0 {
			prefix = Continuation
		}
		out = append(out, prefix+textStyle.Render(line))
	}
	return out
}

// RenderReasoningLines renders agent reasoning as dimmed italic text.
func RenderReasoningLines(text string, width int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	style := StReasoning()
	var out []string
	for _, line := range WrapANSI(text, width-2) {
		out = append(out, "  "+style.Render(line))
	}
	return out
}

// RenderAssistantLines renders an assistant message as markdown. Each returned
// line is guaranteed to fit within width so the differential renderer's row
// accounting stays correct.
func RenderAssistantLines(text string, width int) []string {
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return nil
	}

	// No copy icon: leantui does not hit-test clicks on code blocks.
	rendered, err := markdown.NewRendererWithoutCopyIcon(width).Render(text)
	if err != nil {
		return WrapANSI(text, width)
	}

	var out []string
	for line := range strings.SplitSeq(strings.Trim(rendered, "\n"), "\n") {
		if DisplayWidth(line) > width {
			out = append(out, WrapANSI(line, width)...)
			continue
		}
		out = append(out, line)
	}
	return out
}

func RenderNoticeLines(prefix, text string, width int, style lipgloss.Style) []string {
	wrapped := WrapANSI(text, width-DisplayWidth(prefix))
	out := make([]string, 0, len(wrapped))
	for i, line := range wrapped {
		p := prefix
		if i > 0 {
			p = strings.Repeat(" ", DisplayWidth(prefix))
		}
		out = append(out, style.Render(p+line))
	}
	return out
}
