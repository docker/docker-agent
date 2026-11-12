package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"

	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	tuitypes "github.com/docker/docker-agent/pkg/tui/types"
)

func (t *ToolView) renderWriteFile(msg *tuitypes.Message, ar *animation.Runtime, state service.SessionStateReader, width int) string {
	if args, err := parseWriteFileArgs(msg.ToolCall.Function.Arguments); err == nil {
		t.writeArgs = args
	}
	s := spinner.New(ar, spinner.ModeSpinnerOnly, styles.SpinnerDotsAccentStyle)
	header := toolcommon.RenderTool(msg, s, t.writeArgs.Path, "", width, state.HideToolResults())
	if state.HideToolResults() {
		return header
	}
	if msg.ToolStatus == tuitypes.ToolStatusError {
		return header + "\n" + styles.ToolErrorMessageStyle.Render(msg.Content)
	}
	content := strings.ReplaceAll(t.writeArgs.Content, "\r\n", "\n")
	if content == "" {
		return header
	}
	lines := highlightFileLines(content, t.writeArgs.Path)
	if !t.fullFilePreview && len(lines) > 10 {
		lines = append([]string{"…"}, lines[len(lines)-10:]...)
	}
	availableWidth := max(width-styles.ToolCallResult.GetHorizontalFrameSize(), 1)
	preview := strings.Join(WrapANSI(strings.Join(lines, "\n"), availableWidth), "\n")
	return header + "\n" + styles.ToolCallResult.Render(preview)
}

func parseWriteFileArgs(raw string) (filesystem.WriteFileArgs, error) {
	args, err := toolcommon.ParseArgs[filesystem.WriteFileArgs](raw)
	if err == nil {
		return args, nil
	}
	// A streaming fragment may stop inside an escape sequence, including a Unicode escape.
	inString, escaped := false, false
	escapeStart := -1
	for i := range len(raw) {
		c := raw[i]
		if escaped {
			escaped = false
			if c == 'u' && len(raw)-i < 5 {
				return toolcommon.ParseArgs[filesystem.WriteFileArgs](raw[:escapeStart])
			}
			continue
		}
		if c == '\\' && inString {
			escaped = true
			escapeStart = i
		} else if c == '"' {
			inString = !inString
		}
	}
	if escaped {
		return toolcommon.ParseArgs[filesystem.WriteFileArgs](raw[:escapeStart])
	}
	return args, err
}

func highlightFileLines(content, path string) []string {
	content = strings.TrimSuffix(content, "\n")
	lexer := lexers.Match(path)
	if lexer == nil {
		return strings.Split(content, "\n")
	}
	iterator, err := chroma.Coalesce(lexer).Tokenise(nil, content)
	if err != nil {
		return strings.Split(content, "\n")
	}
	theme := styles.ChromaStyle()
	var highlighted strings.Builder
	for _, token := range iterator.Tokens() {
		entry := theme.Get(token.Type)
		style := lipgloss.NewStyle().TabWidth(-1).Bold(entry.Bold == chroma.Yes).
			Italic(entry.Italic == chroma.Yes).Underline(entry.Underline == chroma.Yes)
		if entry.Colour.IsSet() {
			style = style.Foreground(lipgloss.Color(entry.Colour.String()))
		}
		for i, line := range strings.Split(token.Value, "\n") {
			if i > 0 {
				highlighted.WriteByte('\n')
			}
			highlighted.WriteString(style.Render(line))
		}
	}
	// Some lexers append a newline to incomplete source; don't add a preview row for it.
	lines := strings.Split(highlighted.String(), "\n")
	return lines[:min(len(lines), strings.Count(content, "\n")+1)]
}
