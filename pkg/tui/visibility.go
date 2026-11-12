package tui

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

const (
	tmuxVisibilityInterval = time.Second
	tmuxVisibilityFormat   = "#{window_active_clients} #{window_zoomed_flag} #{pane_active} #{pane_tty}"
	tmuxClientFormat       = "#{client_control_mode} #{P:#{pane_mode} }"
)

type tmuxVisibilityPollMsg struct{}

type tmuxVisibilityMsg struct {
	hidden     bool
	generation uint64
}

func (m *appModel) initTmuxVisibilityCmd() tea.Cmd {
	pane := os.Getenv("TMUX_PANE")
	if os.Getenv("TMUX") == "" || pane == "" || m.tmuxVisibilityProbe != nil {
		return nil
	}
	m.tmuxVisibilityProbe = func(ctx context.Context) bool {
		return tmuxPaneHidden(ctx, pane, os.Stdout)
	}
	return m.checkTmuxVisibilityCmd()
}

func (m *appModel) checkTmuxVisibilityCmd() tea.Cmd {
	if m.tmuxVisibilityProbe == nil {
		return nil
	}
	ctx, probe, generation := m.ctx(), m.tmuxVisibilityProbe, m.visibilityGeneration
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		return tmuxVisibilityMsg{hidden: probe(ctx), generation: generation}
	}
}

func (m *appModel) handleTmuxVisibility(msg tmuxVisibilityMsg) tea.Cmd {
	var cmds []tea.Cmd
	if msg.generation == m.visibilityGeneration {
		cmds = append(cmds, m.setPaneHidden(msg.hidden))
	}
	if m.tmuxVisibilityProbe != nil {
		// Poll independently of focus-events: an unfocused split can become visible.
		cmds = append(cmds, tea.Tick(tmuxVisibilityInterval, func(time.Time) tea.Msg {
			return tmuxVisibilityPollMsg{}
		}))
	}
	return tea.Batch(cmds...)
}

func (m *appModel) setPaneHidden(hidden bool) tea.Cmd {
	if m.paneHidden == hidden {
		return nil
	}
	m.paneHidden = hidden
	m.viewCacheValid = false
	if hidden {
		m.ar.Pause()
		return nil
	}
	return m.ar.Resume()
}

func tmuxPaneHidden(ctx context.Context, pane string, output *os.File) bool {
	out, err := exec.CommandContext(ctx, "tmux",
		"display-message", "-p", "-t", pane, tmuxVisibilityFormat,
		";", "list-clients", "-F", tmuxClientFormat,
	).Output()
	return err == nil && tmuxVisibilityHidden(string(out), output)
}

func tmuxVisibilityHidden(out string, output *os.File) bool {
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	// Control frontends and chooser previews can display otherwise hidden panes.
	for _, client := range lines[1:] {
		fields := strings.Fields(client)
		if len(fields) == 0 || fields[0] != "0" {
			return false
		}
		for _, mode := range fields[1:] {
			switch mode {
			case "copy-mode", "view-mode", "buffer-mode":
			default:
				return false
			}
		}
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 4 || (fields[1] != "0" && fields[1] != "1") || (fields[2] != "0" && fields[2] != "1") {
		return false
	}
	clients, err := strconv.Atoi(fields[0])
	if err != nil || clients < 0 {
		return false
	}
	// Inactive split panes are visible unless another pane is zoomed.
	if clients > 0 && (fields[1] == "0" || fields[2] == "1") {
		return false
	}
	// GUI terminals can inherit TMUX from an unrelated pane.
	paneTTY, err := os.Stat(fields[3])
	if err != nil {
		return false
	}
	ourTTY, err := output.Stat()
	return err == nil && os.SameFile(paneTTY, ourTTY)
}
