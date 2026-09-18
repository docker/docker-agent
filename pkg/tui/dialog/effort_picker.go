package dialog

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	effortPickerWidthPercent = 40
	// Wide enough that the three help-key bindings fit on a single line.
	effortPickerMinWidth = 46
	effortPickerMaxWidth = 50
)

// effortPickerDialog lets the user pick a reasoning-effort level for the
// current model among the levels that model supports. It backs /effort when
// no level argument is given.
type effortPickerDialog struct {
	BaseDialog

	levels   []effort.Level
	current  effort.Level
	selected int
}

// NewEffortPickerDialog creates an effort picker over the given supported
// levels. current marks the active level and is preselected; pass an empty
// level when the active configuration doesn't map onto a listed level
// (adaptive or token-based budgets).
func NewEffortPickerDialog(levels []effort.Level, current effort.Level) Dialog {
	d := &effortPickerDialog{levels: levels, current: current}
	for i, l := range levels {
		if l == current {
			d.selected = i
			break
		}
	}
	return d
}

func (d *effortPickerDialog) Init() tea.Cmd { return nil }

func (d *effortPickerDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}
		cmd := d.handleKey(msg)
		return d, cmd
	}
	return d, nil
}

var effortPickerKeys = struct {
	Close, Up, Down, Home, End, Enter key.Binding
}{
	key.NewBinding(key.WithKeys("esc", "q")),
	key.NewBinding(key.WithKeys("up", "k", "ctrl+k")),
	key.NewBinding(key.WithKeys("down", "j", "ctrl+j")),
	key.NewBinding(key.WithKeys("home", "g")),
	key.NewBinding(key.WithKeys("end", "G")),
	key.NewBinding(key.WithKeys("enter")),
}

func (d *effortPickerDialog) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, effortPickerKeys.Close):
		return closeDialogCmd()
	case key.Matches(msg, effortPickerKeys.Up):
		if d.selected > 0 {
			d.selected--
		}
	case key.Matches(msg, effortPickerKeys.Down):
		if d.selected < len(d.levels)-1 {
			d.selected++
		}
	case key.Matches(msg, effortPickerKeys.Home):
		d.selected = 0
	case key.Matches(msg, effortPickerKeys.End):
		d.selected = max(0, len(d.levels)-1)
	case key.Matches(msg, effortPickerKeys.Enter):
		return d.handleSelection()
	}
	return nil
}

func (d *effortPickerDialog) handleSelection() tea.Cmd {
	if d.selected < 0 || d.selected >= len(d.levels) {
		return nil
	}
	return tea.Sequence(
		closeDialogCmd(),
		core.CmdHandler(messages.SetThinkingLevelMsg{Level: d.levels[d.selected].String()}),
	)
}

func (d *effortPickerDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

func (d *effortPickerDialog) View() string {
	width := d.ComputeDialogWidth(effortPickerWidthPercent, effortPickerMinWidth, effortPickerMaxWidth)
	inner := d.ContentWidth(width, 2)

	rows := make([]string, 0, len(d.levels))
	for i, level := range d.levels {
		rows = append(rows, d.renderLevel(level, i == d.selected))
	}

	content := NewContent(inner).
		AddTitle("Select Reasoning Effort").
		AddSeparator().
		AddSpace().
		AddContent(lipgloss.JoinVertical(lipgloss.Left, rows...)).
		AddSpace().
		AddHelpKeys("↑/↓", "navigate", "enter", "select", "esc", "cancel").
		Build()

	return styles.DialogStyle.Width(width).Render(content)
}

// renderLevel draws one list entry: the six-cell effort gauge (empty for
// none, matching the sidebar's visual language), the level name, and a
// "(current)" badge on the active level.
func (d *effortPickerDialog) renderLevel(level effort.Level, selected bool) string {
	nameStyle := styles.PaletteUnselectedActionStyle
	currentBadgeStyle := styles.BadgeCurrentStyle
	if selected {
		nameStyle = styles.PaletteSelectedActionStyle
		currentBadgeStyle = currentBadgeStyle.Background(styles.MobyBlue)
	}

	name := nameStyle.Render(" " + level.String() + " ")
	if level == d.current {
		name += currentBadgeStyle.Render("(current)")
	}
	return toolcommon.EffortGauge(level) + " " + name
}
