package ui

import (
	"strings"

	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// Screen aggregates the lean TUI presentation models and lays out a full frame.
type Screen struct {
	Transcript   *Transcript
	Editor       *Editor
	Autocomplete *Autocomplete
	Status       StatusModel
	Confirm      *ConfirmModel
	Settings     *SettingsModel
}

func NewScreen(workingDir, branch, editorPlaceholder string, historyStore ...*history.History) *Screen {
	return &Screen{
		Transcript:   NewTranscript(),
		Editor:       NewEditor(editorPlaceholder, historyStore...),
		Autocomplete: NewAutocomplete(),
		Status:       StatusModel{WorkingDir: workingDir, Branch: branch},
	}
}

// Frame produces the full terminal frame and cursor position.
func (s *Screen) Frame(width, _, spinnerFrame int, busy bool, sessionState service.SessionStateReader, pendingUsers []PendingUserMessage) (lines []string, cursorLine, cursorCol int) {
	lines = s.Transcript.Lines(width, spinnerFrame, busy, sessionState, pendingUsers)

	lines = append(lines, s.Autocomplete.Render(width)...)
	lines = append(lines, renderInputSeparator(width, spinnerFrame, busy))

	inputStart := len(lines)
	switch {
	case s.Confirm != nil:
		confirmLines := s.Confirm.Render(width)
		lines = append(lines, confirmLines...)
		cursorLine = inputStart + max(len(confirmLines)-1, 0)
		if len(confirmLines) > 0 {
			cursorCol = min(DisplayWidth(confirmLines[len(confirmLines)-1]), max(width-1, 0))
		}
	case s.Settings != nil:
		settingsLines := s.Settings.Render(width)
		lines = append(lines, settingsLines...)
		cursorLine = inputStart + len(settingsLines) - 1
	default:
		editorLines, row, col := s.Editor.Layout(width)
		lines = append(lines, editorLines...)
		cursorLine = inputStart + row
		cursorCol = col
	}

	lines = append(lines, "")
	lines = append(lines, RenderStatus(s.Status, width)...)

	return lines, cursorLine, cursorCol
}

func renderInputSeparator(width, spinnerFrame int, busy bool) string {
	width = max(width, 0)
	if !busy {
		return StMuted().Render(strings.Repeat("─", width))
	}

	label := StMuted().Render("── ") + spinnerLine(spinnerFrame) + " "
	return Truncate(label+StMuted().Render(strings.Repeat("─", max(width-DisplayWidth(label), 0))), width)
}

// ConfirmModel holds a pending tool-approval prompt.
type ConfirmModel struct {
	Tool string
	View ToolView
}

func (c *ConfirmModel) Render(width int) []string {
	lines := []string{Truncate(StWarning().Render("● Approve tool call"), width)}
	lines = append(lines, RenderTool(c.View, width)...)
	lines = append(lines, Truncate(StMuted().Render("[y] yes   [a] always this tool   [b] auto-approve safe   [s] whole session   [n] no"), width))
	return lines
}
