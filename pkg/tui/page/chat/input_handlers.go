package chat

import (
	"context"
	"errors"
	"log/slog"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/components/tool/editfile"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// handleKeyPress handles keyboard input events for the chat page.
// Returns the updated model and command. All key presses are handled (forwarded to messages if no match).
func (p *chatPage) handleKeyPress(msg tea.KeyPressMsg) (layout.Model, tea.Cmd) {
	// When editing title, route keypresses to the sidebar
	if p.sidebar.IsEditingTitle() {
		switch msg.Key().Code {
		case tea.KeyEnter:
			newTitle := p.sidebar.CommitTitleEdit()
			cmd := p.persistSessionTitle(newTitle)
			focusCmd := core.CmdHandler(msgtypes.RequestFocusMsg{Target: msgtypes.PanelEditor})
			return p, tea.Batch(cmd, focusCmd)
		case tea.KeyEscape:
			p.sidebar.CancelTitleEdit()
			return p, core.CmdHandler(msgtypes.RequestFocusMsg{Target: msgtypes.PanelEditor})
		default:
			cmd := p.sidebar.UpdateTitleInput(msg)
			return p, cmd
		}
	}

	switch {
	case key.Matches(msg, p.keyMap.Cancel):
		// If inline editing is active, cancel the edit first
		if p.messages.IsInlineEditing() {
			cmd := p.messages.CancelInlineEdit()
			return p, cmd
		}
		// Otherwise cancel the stream (only if something is running)
		if p.working || p.msgCancel != nil {
			cmd := p.cancelStream(true)
			return p, cmd
		}
		// Forward to messages for other uses (e.g., clear selection)
		model, cmd := p.messages.Update(msg)
		p.messages = model.(messages.Model)
		return p, cmd

	case key.Matches(msg, p.keyMap.ToggleSplitDiff):
		p.sessionState.ToggleSplitDiffView()
		model, cmd := p.messages.Update(editfile.ToggleDiffViewMsg{})
		p.messages = model.(messages.Model)
		return p, cmd

	case key.Matches(msg, p.keyMap.ToggleSidebar):
		p.sidebar.ToggleCollapsed()
		cmd := p.SetSize(p.width, p.height)
		return p, tea.Batch(cmd, core.CmdHandler(msgtypes.ToggleSidebarMsg{}))
	}

	// Route keys to messages (for scrolling, etc.)
	model, cmd := p.messages.Update(msg)
	p.messages = model.(messages.Model)
	return p, cmd
}

// persistSessionTitle saves the new session title to the store
func (p *chatPage) persistSessionTitle(newTitle string) tea.Cmd {
	return func() tea.Msg {
		if err := p.app.UpdateSessionTitle(context.Background(), newTitle); err != nil {
			// Show warning if title generation is in progress
			if errors.Is(err, app.ErrTitleGenerating) {
				return notification.ShowMsg{Text: "Title is being generated, please wait", Type: notification.TypeWarning}
			}
			slog.Warn("Failed to persist session title", "title", newTitle, "error", err)
			return nil
		}
		return nil
	}
}

// copyWorkingDirToClipboard copies the working directory path to the system clipboard.
func copyWorkingDirToClipboard(wd string) tea.Cmd {
	return tea.Sequence(
		func() tea.Msg {
			_ = clipboard.WriteAll(wd)
			return nil
		},
		tea.SetClipboard(wd),
		notification.SuccessCmd("Working directory copied to clipboard."),
	)
}

// handleMouseClick handles mouse click events.
func (p *chatPage) handleMouseClick(msg tea.MouseClickMsg) (layout.Model, tea.Cmd) {
	hit := NewHitTest(p)
	target := hit.At(msg.X, msg.Y)

	switch target {
	case TargetSidebarToggle:
		if msg.Button == tea.MouseLeft {
			p.sidebar.ToggleCollapsed()
			cmd := p.SetSize(p.width, p.height)
			return p, tea.Batch(cmd, core.CmdHandler(msgtypes.ToggleSidebarMsg{}))
		}

	case TargetSidebarResizeHandle:
		if msg.Button == tea.MouseLeft {
			p.isDraggingSidebar = true
			p.sidebarDragStartX = msg.X
			p.sidebarDragStartWidth = p.sidebar.GetPreferredWidth()
			p.sidebarDragMoved = false
			return p, nil
		}

	case TargetSidebarStar:
		if msg.Button == tea.MouseLeft {
			sess := p.app.Session()
			if sess != nil {
				return p, core.CmdHandler(msgtypes.ToggleSessionStarMsg{SessionID: sess.ID})
			}
			return p, nil
		}

	case TargetSidebarTitle:
		// Double-click on title to edit
		if msg.Button == tea.MouseLeft {
			if p.sidebar.HandleTitleClick() {
				p.sidebar.BeginTitleEdit()
				return p, core.CmdHandler(msgtypes.RequestFocusMsg{Target: msgtypes.PanelSidebarTitle})
			}
			return p, nil
		}

	case TargetSidebarWorkingDir:
		if msg.Button == tea.MouseLeft {
			return p, copyWorkingDirToClipboard(p.sidebar.WorkingDirectory())
		}

	case TargetSidebarAgent:
		if msg.Button == tea.MouseLeft {
			if hit.AgentName != "" {
				return p, core.CmdHandler(msgtypes.SwitchAgentMsg{AgentName: hit.AgentName})
			}
			return p, nil
		}

	case TargetMessages:
		if !p.messages.IsMouseOnScrollbar(msg.X, msg.Y) {
			cmd := p.routeMouseEvent(msg, msg.Y)
			focusCmd := core.CmdHandler(msgtypes.RequestFocusMsg{
				Target: msgtypes.PanelMessages,
				ClickX: msg.X,
				ClickY: msg.Y,
			})
			return p, tea.Batch(cmd, focusCmd)
		}
	}

	// Default: route to appropriate component
	cmd := p.routeMouseEvent(msg, msg.Y)
	return p, cmd
}

// handleMouseMotion handles mouse motion events.
func (p *chatPage) handleMouseMotion(msg tea.MouseMotionMsg) (layout.Model, tea.Cmd) {
	if p.isDraggingSidebar {
		delta := p.sidebarDragStartX - msg.X
		if max(delta, -delta) >= dragThreshold {
			p.sidebarDragMoved = true
		}
		if p.sidebarDragMoved {
			cmd := p.handleSidebarResize(msg.X)
			return p, cmd
		}
		return p, nil
	}

	// During a scrollbar drag, forward motion to both scrollable components
	// so the drag continues even when the cursor drifts outside the component.
	// The scrollbar ignores motion if it isn't the one being dragged.
	if p.isScrollbarDragging() {
		var cmds []tea.Cmd
		messagesModel, messagesCmd := p.messages.Update(msg)
		p.messages = messagesModel.(messages.Model)
		cmds = append(cmds, messagesCmd)

		sidebarModel, sidebarCmd := p.sidebar.Update(msg)
		p.sidebar = sidebarModel.(sidebar.Model)
		cmds = append(cmds, sidebarCmd)
		return p, tea.Batch(cmds...)
	}

	cmd := p.routeMouseEvent(msg, msg.Y)
	return p, cmd
}

// handleMouseRelease handles mouse release events.
// Release is broadcast to all scrollable components so that a scrollbar drag
// that ends outside the component's bounds still terminates correctly.
func (p *chatPage) handleMouseRelease(msg tea.MouseReleaseMsg) (layout.Model, tea.Cmd) {
	if p.isDraggingSidebar {
		p.isDraggingSidebar = false
		cmd := p.SetSize(p.width, p.height)
		return p, cmd
	}

	var cmds []tea.Cmd

	// Forward release to both messages and sidebar so any active scrollbar
	// drag is properly ended, regardless of where the mouse was released.
	messagesModel, messagesCmd := p.messages.Update(msg)
	p.messages = messagesModel.(messages.Model)
	cmds = append(cmds, messagesCmd)

	sidebarModel, sidebarCmd := p.sidebar.Update(msg)
	p.sidebar = sidebarModel.(sidebar.Model)
	cmds = append(cmds, sidebarCmd)

	return p, tea.Batch(cmds...)
}

// isScrollbarDragging returns true if any scrollable component has an active scrollbar drag.
func (p *chatPage) isScrollbarDragging() bool {
	return p.messages.IsScrollbarDragging() || p.sidebar.IsScrollbarDragging()
}

// handleMouseWheel handles mouse wheel events.
func (p *chatPage) handleWheelCoalesced(msg msgtypes.WheelCoalescedMsg) (layout.Model, tea.Cmd) {
	if msg.Delta == 0 {
		return p, nil
	}
	switch p.wheelTarget(msg.X, msg.Y) {
	case wheelTargetSidebar:
		model, cmd := p.sidebar.Update(msg)
		p.sidebar = model.(sidebar.Model)
		return p, cmd
	default:
		model, cmd := p.messages.Update(msg)
		p.messages = model.(messages.Model)
		return p, cmd
	}
}

type wheelTarget int

const (
	wheelTargetMessages wheelTarget = iota
	wheelTargetSidebar
)

func (p *chatPage) wheelTarget(x, _ int) wheelTarget {
	sl := p.computeSidebarLayout()
	if sl.mode == sidebarVertical && !p.sidebar.IsCollapsed() {
		adjustedX := x - styles.AppPadding
		if sl.isInSidebar(adjustedX) {
			return wheelTargetSidebar
		}
	}

	return wheelTargetMessages
}

// handleSidebarResize adjusts sidebar width based on drag position.
func (p *chatPage) handleSidebarResize(x int) tea.Cmd {
	innerWidth := p.width - appPaddingHorizontal
	delta := p.sidebarDragStartX - x
	newWidth := p.sidebarDragStartWidth + delta

	// Auto-collapse if dragged below minimum
	if newWidth < sidebar.MinWidth {
		if !p.sidebar.IsCollapsed() {
			// Set preferredWidth to 0 so expanding resets to default
			p.sidebar.SetPreferredWidth(0)
			p.sidebar.SetCollapsed(true)
			return tea.Batch(p.SetSize(p.width, p.height), core.CmdHandler(msgtypes.ToggleSidebarMsg{}))
		}
		return nil
	}

	// Auto-expand if dragged back above minimum
	var cmds []tea.Cmd
	if p.sidebar.IsCollapsed() {
		p.sidebar.SetCollapsed(false)
		cmds = append(cmds, core.CmdHandler(msgtypes.ToggleSidebarMsg{}))
	}

	newWidth = p.sidebar.ClampWidth(newWidth, innerWidth)
	if newWidth != p.sidebar.GetPreferredWidth() {
		p.sidebar.SetPreferredWidth(newWidth)
		cmds = append(cmds, p.SetSize(p.width, p.height))
	}
	return tea.Batch(cmds...)
}
