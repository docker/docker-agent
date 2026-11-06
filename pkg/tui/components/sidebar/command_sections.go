package sidebar

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/shellpath"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	// commandSectionInterval is how long a command section waits between runs.
	commandSectionInterval = 5 * time.Second
	// commandSectionTimeout bounds one run, so a hung command cannot stall the next.
	commandSectionTimeout = 5 * time.Second
	// commandSectionMaxLines keeps a chatty command from pushing the other sections away.
	commandSectionMaxLines = 12
)

// CommandSection is a sidebar section whose body is the output of a shell
// command, run again every few seconds.
type CommandSection struct {
	Title   string
	Command string
}

// ParseCommandSection reads a "Title=command" flag value.
func ParseCommandSection(spec string) (CommandSection, error) {
	title, command, ok := strings.Cut(spec, "=")
	title, command = strings.TrimSpace(title), strings.TrimSpace(command)
	if !ok || title == "" || command == "" {
		return CommandSection{}, errors.New(`a sidebar section is written "Title=command"`)
	}
	return CommandSection{Title: title, Command: command}, nil
}

type commandSectionState struct {
	CommandSection

	lines []string
}

type commandSectionResultMsg struct {
	index int
	lines []string
}

type commandSectionDueMsg struct{ index int }

func (m *model) SetCommandSections(sections []CommandSection) {
	m.commandSections = make([]commandSectionState, len(sections))
	for i, section := range sections {
		m.commandSections[i].CommandSection = section
	}
	m.invalidateCache()
}

// pollCommandSections starts every command section's first run. Init runs
// on every tab switch, so only the first call starts the chain.
func (m *model) pollCommandSections() tea.Cmd {
	if m.commandSectionsPolling || len(m.commandSections) == 0 {
		return nil
	}
	m.commandSectionsPolling = true
	cmds := make([]tea.Cmd, len(m.commandSections))
	for i := range m.commandSections {
		cmds[i] = m.runCommandSection(i)
	}
	return tea.Batch(cmds...)
}

func (m *model) runCommandSection(index int) tea.Cmd {
	if index < 0 || index >= len(m.commandSections) {
		return nil
	}
	command, parent := m.commandSections[index].Command, m.ctx()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, commandSectionTimeout)
		defer cancel()
		shell, args := shellpath.DetectShell()
		// A failing command still reports what it printed; none means no section.
		out, _ := exec.CommandContext(ctx, shell, append(args, command)...).Output()
		return commandSectionResultMsg{index: index, lines: commandSectionLines(string(out))}
	}
}

// commandSectionLines keeps the printable text of a command's output.
func commandSectionLines(out string) []string {
	var lines []string
	for line := range strings.SplitSeq(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(strings.ReplaceAll(ansi.Strip(line), "\t", " "), " ")
		if line == "" {
			continue
		}
		if len(lines) == commandSectionMaxLines {
			lines[len(lines)-1] = "…"
			break
		}
		lines = append(lines, line)
	}
	return lines
}

// handleCommandSection applies a finished run and schedules the next one.
func (m *model) handleCommandSection(msg commandSectionResultMsg) tea.Cmd {
	if msg.index < 0 || msg.index >= len(m.commandSections) {
		return nil
	}
	if state := &m.commandSections[msg.index]; !slices.Equal(state.lines, msg.lines) {
		state.lines = msg.lines
		m.invalidateCache()
	}
	index := msg.index
	return tea.Tick(commandSectionInterval, func(time.Time) tea.Msg {
		return commandSectionDueMsg{index: index}
	})
}

// commandSection renders one command section, or "" while it has no output.
func (m *model) commandSection(state commandSectionState, contentWidth int) string {
	if len(state.lines) == 0 {
		return ""
	}
	lines := make([]string, len(state.lines))
	for i, line := range state.lines {
		lines[i] = styles.BaseStyle.Render(ansi.Truncate(line, contentWidth, "…"))
	}
	return m.renderTab(state.Title, strings.Join(lines, "\n"), contentWidth)
}
