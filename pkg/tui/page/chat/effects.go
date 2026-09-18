package chat

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
)

// Effects separates page work from commands that require the visible UI.
// Commands keep their native Bubble Tea composition; never wrap an opaque
// command's result, which may be a Sequence or a terminal-control message.
type Effects struct {
	// Local work runs even when hidden and routes its results to the owning page.
	Local tea.Cmd
	// Visible commands drive chrome, notifications and dialogs. Hidden attention
	// dialogs are replayed from the tab's FIFO, not from this command.
	Visible tea.Cmd
	// Global effects target the application regardless of the selected tab.
	Global tea.Cmd
}

func (e Effects) Cmd(visible bool) tea.Cmd {
	if visible {
		return tea.Batch(e.Local, e.Visible, e.Global)
	}
	return tea.Batch(e.Local, e.Global)
}

// tabLocal classifies already-routed work during UpdateEffects. Outside an
// update (e.g. Init), the caller dispatches the command directly.
// Call only from unordered Batch paths, never from inside a Sequence.
func (p *chatPage) tabLocal(cmd tea.Cmd) tea.Cmd {
	if p.effects == nil {
		return cmd
	}
	p.effects.Local = tea.Batch(p.effects.Local, cmd)
	return nil
}

// GlobalMsg is an application effect owned by a page lifetime. The host checks
// Origin before applying Inner, but does not require the tab to be visible.
// Inner must be an application message, not a Bubble Tea control message.
type GlobalMsg struct {
	TabID       string
	Origin      Page
	Application *app.App
	Inner       tea.Msg
}

func (p *chatPage) global(cmd tea.Cmd) tea.Cmd {
	if p.effects == nil {
		return cmd
	}
	p.effects.Global = tea.Batch(p.effects.Global, cmd)
	return nil
}
