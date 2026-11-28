package tui

import (
	"os"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/components/tabbar"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/internal/editorname"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

func (m *appModel) openHelp() (tea.Model, tea.Cmd) {
	if m.dialogMgr.HasDialog(dialog.IsHelpDialog) {
		return m, nil
	}
	return m, core.CmdHandler(dialog.OpenDialogMsg{Model: dialog.NewHelpDialog(m.helpSections())})
}

func shortcut(desc string, keys ...string) key.Binding {
	return key.NewBinding(key.WithKeys(keys...), key.WithHelp("", desc))
}

// describe keeps the configured keys and changes only their Help text.
func describe(b key.Binding, text string) key.Binding {
	b.SetHelp("", text)
	return b
}

// helpBindings removes keys consumed earlier in the input route without
// modifying the configured binding or hiding any aliases that still work.
func helpBindings(bindings []key.Binding, earlier ...key.Binding) []key.Binding {
	var result []key.Binding
	for _, b := range bindings {
		if !b.Enabled() {
			continue
		}
		keys := slices.DeleteFunc(slices.Clone(b.Keys()), func(k string) bool {
			return slices.ContainsFunc(earlier, func(e key.Binding) bool {
				return e.Enabled() && slices.Contains(e.Keys(), k)
			})
		})
		if len(keys) > 0 {
			b.SetKeys(keys...)
			result = append(result, b)
		}
	}
	return result
}

func (m *appModel) helpSections() []dialog.HelpSection {
	keys := core.GetKeys()
	quit := describe(keys.Quit, "Exit TUI")
	if m.leanMode {
		return []dialog.HelpSection{{Title: "Application", Bindings: []key.Binding{quit}}}
	}

	ed := m.activeTab.editor
	page := m.activeTab.chatPage
	modal := m.dialogMgr.Open()
	history := ed.IsHistorySearchActive()
	completing := !modal && m.completions.Open()
	var sections []dialog.HelpSection
	tabKeys := tabbar.DefaultKeyMap()
	tabBindings := []key.Binding{tabKeys.NewTab, tabKeys.PrevTab, tabKeys.NextTab}
	closeTab := m.tabBar.CanCloseTabWithKey() && (m.focusedPanel != PanelEditor || m.dialogMgr.TopIsBackground())
	if closeTab {
		tabBindings = append(tabBindings, tabKeys.CloseTab)
	}
	precedence := []key.Binding{keys.Quit}
	if m.isTranscribing() {
		send := shortcut("Stop transcription and send", "enter")
		stop := shortcut("Stop transcription", "esc")
		sections = append(sections, dialog.HelpSection{Title: "Transcription", Bindings: []key.Binding{send, stop}})
		precedence = append(precedence, send, stop)
	}

	global := []key.Binding{keys.Suspend, keys.Commands, keys.ToggleYolo, keys.ToggleHideToolResults, keys.CycleAgent, keys.ModelPicker, keys.ClearQueue}
	focus := describe(keys.SwitchFocus, "Switch focus, unless completing an argument or accepting a suggestion")
	external := describe(keys.EditExternal, "Edit draft in "+editorname.FromEnv(os.Getenv("VISUAL"), os.Getenv("EDITOR")))
	thinking := shortcut("Cycle thinking level (when supported)", "shift+tab")
	interrupt := shortcut("Interrupt response or clear selection", "esc")
	agent := shortcut("Switch directly to an agent by number", "ctrl+1", "ctrl+2", "ctrl+3", "ctrl+4", "ctrl+5", "ctrl+6", "ctrl+7", "ctrl+8", "ctrl+9")
	help := keys.Help
	commandPalette := describe(keys.Commands, "Open command palette")
	beforeTabs := slices.Clone(precedence)
	if closeTab && !history {
		beforeTabs = append(beforeTabs, tabKeys.CloseTab)
	}
	beforeTour := append(slices.Clone(beforeTabs), global...)
	beforeTour = append(beforeTour, keys.Help)
	chatPrecedence := beforeTour
	if m.tour.Active() {
		tourKeys := []key.Binding{shortcut("Quit the getting-started tour", "esc")}
		if m.focusedPanel == PanelEditor && strings.TrimSpace(ed.Value()) == "" {
			tourKeys = append(tourKeys, shortcut("Advance the tour", "enter"))
		}
		if !modal && !completing && !history {
			sections = append(sections, dialog.HelpSection{Title: "Tour", Bindings: helpBindings(tourKeys, chatPrecedence...)})
		}
		chatPrecedence = append(chatPrecedence, tourKeys...)
	}

	// Context always describes the layer under Help, not Help itself.
	beforePanel := append(slices.Clone(chatPrecedence), external, keys.HistorySearch, keys.ToggleSidebar, thinking, focus, agent)
	composer := composerHelp(keys, m.keyboardEnhancementsSupported, beforePanel)
	composer = append(composer, helpBindings([]key.Binding{external}, chatPrecedence...)...)
	if !ed.IsRecording() {
		composer = append(composer, helpBindings([]key.Binding{keys.HistorySearch}, chatPrecedence...)...)
	}
	composer = append(composer, helpBindings([]key.Binding{describe(keys.ClearQueue, "Clear queued messages")}, beforeTabs...)...)
	transcript := page.Bindings()
	if page.IsInlineEditing() {
		transcript = nil
	}
	transcript = append(slices.Clone(transcript), shortcut("Scroll by a page", "pgup", "pgdown"), shortcut("Jump to top or bottom", "home", "end"), shortcut("Restore queued messages to composer", "alt+up"))
	transcript = helpBindings(transcript, beforePanel...)
	transcript = append(transcript, helpBindings([]key.Binding{describe(keys.ToggleHideToolResults, "Show or hide tool results")}, precedence...)...)
	if !chat.IsTitleEditing(page) && !page.IsInlineEditing() {
		transcript = append(transcript, helpBindings([]key.Binding{interrupt}, chatPrecedence...)...)
	}
	var context dialog.HelpSection
	switch {
	case modal:
		context = dialog.ContextHelp(m.dialogMgr.TopDialog())
		modalHelp := shortcut("Open help over a dialog", "f1")
		context.Bindings = helpBindings(context.Bindings, append(slices.Clone(precedence), modalHelp)...)
		if m.dialogMgr.TopIsExitConfirmation() {
			quit.SetHelp("", "Confirm exit")
		}
		help = modalHelp
	case completing:
		popup := []key.Binding{
			shortcut("Move through candidates", "up", "down"),
			shortcut("Select and submit", "enter"),
			shortcut("Complete without submitting", "tab"),
			shortcut("Dismiss completion", "esc"),
		}
		popupEarlier := append(slices.Clone(precedence), keys.Help)
		context = dialog.HelpSection{Title: "Completion popup", Note: "Keep typing to filter candidates.", Bindings: helpBindings(popup, popupEarlier...)}
	case history && m.focusedPanel == PanelEditor:
		context = dialog.HelpSection{Title: "History search", Bindings: helpBindings([]key.Binding{
			shortcut("Browse matches", "up", "down", "ctrl+p", "ctrl+n"),
			shortcut("Use matching draft", "enter"),
			shortcut("Cancel search", "esc", "ctrl+g"),
		}, beforeTour...)}
	case m.focusedPanel == PanelContent && chat.IsTitleEditing(page):
		context = dialog.HelpSection{Title: "Session title", Bindings: helpBindings([]key.Binding{shortcut("Save title", "enter"), shortcut("Cancel edit", "esc")}, beforePanel...)}
	case m.focusedPanel == PanelContent && page.IsInlineEditing():
		context = dialog.HelpSection{Title: "Inline message editing", Bindings: helpBindings(page.Bindings(), beforePanel...)}
	case m.focusedPanel == PanelContent:
		context = dialog.HelpSection{Title: "Transcript", Bindings: transcript}
	case m.focusedPanel == PanelAttachments:
		context = dialog.HelpSection{Title: "Attachments", Bindings: helpBindings([]key.Binding{
			describe(keys.EditorSend, "Expand or collapse attachments"),
			shortcut("Return to the composer", "esc"),
		}, beforePanel...)}
	default:
		context = dialog.HelpSection{Title: "Composer", Bindings: composer}
	}
	appKeys := append(helpBindings([]key.Binding{quit}, precedence[1:]...), helpBindings([]key.Binding{help}, precedence...)...)
	if !modal && !completing {
		appKeys = append(appKeys, helpBindings([]key.Binding{commandPalette, keys.Suspend}, precedence...)...)
		if !history {
			appKeys = append(appKeys, helpBindings([]key.Binding{focus}, chatPrecedence...)...)
			if !m.hideSidebar {
				appKeys = append(appKeys, helpBindings([]key.Binding{keys.ToggleSidebar}, chatPrecedence...)...)
			}
		}
	}
	sections = append(sections, context, dialog.HelpSection{Title: "Application", Bindings: appKeys})

	tabs := dialog.HelpSection{Title: "Tabs", Bindings: tabBindings}
	switch {
	case history:
		tabs.Note = "After leaving history search."
	case modal && !m.dialogMgr.TopIsBackground():
		tabs.Note = "After closing the dialog."
	}
	sections = append(sections, tabs)

	agents := dialog.HelpSection{Title: "Agent and model", Bindings: helpBindings([]key.Binding{
		describe(keys.ModelPicker, "Choose model"),
		describe(keys.ToggleYolo, "Toggle automatic tool approval (YOLO mode)"),
		keys.CycleAgent,
	}, precedence...)}
	if !history {
		agents.Bindings = append(agents.Bindings, helpBindings([]key.Binding{thinking, agent}, chatPrecedence...)...)
	}
	switch {
	case modal:
		agents.Note = "After closing the dialog."
	case completing:
		agents.Note = "After dismissing completion."
	}
	sections = append(sections, agents)
	if context.Title != "Composer" {
		sections = append(sections, dialog.HelpSection{Title: "Composer", Note: "While writing a message.", Bindings: composer})
	}
	if context.Title != "Transcript" {
		if !slices.ContainsFunc(transcript, func(b key.Binding) bool { return slices.Contains(b.Keys(), "e") }) {
			transcript = append(transcript, helpBindings([]key.Binding{shortcut("Edit selected user message (when available)", "e")}, beforePanel...)...)
		}
		sections = append(sections, dialog.HelpSection{Title: "Transcript", Note: "Focus the transcript to navigate messages.", Bindings: transcript})
	}
	return sections
}

func composerHelp(keys core.KeyMap, enhanced bool, earlier []key.Binding) []key.Binding {
	newlineKeys := slices.Clone(keys.EditorNewline.Keys())
	if enhanced && !slices.Contains(newlineKeys, "shift+enter") {
		newlineKeys = append([]string{"shift+enter"}, newlineKeys...)
	}
	input := textarea.DefaultKeyMap()
	followUp := shortcut("Queue an end-of-turn follow-up", "alt+enter")
	composer := helpBindings([]key.Binding{followUp}, append(slices.Clone(earlier), input.Paste, input.DeleteCharacterBackward)...)
	beforeSend := append(slices.Clone(earlier), input.Paste, input.DeleteCharacterBackward, followUp)
	composer = append(composer, helpBindings([]key.Binding{keys.EditorSend}, beforeSend...)...)
	composer = append(composer, helpBindings([]key.Binding{shortcut("Insert newline", newlineKeys...)}, append(beforeSend, keys.EditorSend)...)...)
	other := []key.Binding{shortcut("Browse input history before typing; otherwise move the cursor", "up", "down"), input.DeleteWordBackward}
	return append(composer, helpBindings(other, append(beforeSend, keys.EditorSend, keys.EditorNewline)...)...)
}
