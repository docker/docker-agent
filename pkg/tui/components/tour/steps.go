package tour

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/cli/invocation"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// buildSteps returns the tour's scripted steps. The card copy is rendered by
// the TUI itself (deterministic, no tokens); only the actions the user
// performs go through the real agent.
func buildSteps() []step {
	return []step{
		{
			title:     "Talk to your agent",
			body:      "This is a conversation, not a form. Plain language works.",
			action:    "Type anything in the chat and press Enter. Try \"write a haiku about containers\".",
			check:     checkMessageSent,
			celebrate: "Message away. The reply streams in live.",
		},
		{
			title:     "Tools run with your OK",
			body:      "Agents get real work done with tools: shell, files, fetch. Anything with side effects waits for your approval.",
			action:    "Ask \"run ls in the shell\" and approve the tool call. (Ctrl+y toggles yolo: auto-approve)",
			check:     checkToolCallApproved,
			celebrate: "That's the approval flow. You decide what runs.",
		},
		{
			title:     "One menu for everything",
			body:      "The command palette holds every feature: searchable, nothing to memorize.",
			action:    "Press Ctrl+k and look around. (Esc closes it)",
			check:     checkPaletteOpened,
			celebrate: "You found the control room.",
		},
		{
			title:     "Slash commands",
			body:      "Prefer typing? Slash commands are the shortcut: /model, /sessions, /theme, /compact…",
			action:    "Try /model to see and switch the current model.",
			check:     checkSlashCommandUsed,
			celebrate: "Slash commands unlocked.",
		},
		{
			title: "Agents are just YAML",
			body: "The agent you're talking to is one YAML file: a model, instructions, and tools.\n\n" +
				fmt.Sprintf("Build your own:\n  %s new\n\n", invocation.DockerAgent()) +
				fmt.Sprintf("Or run one from an OCI registry:\n  %s run myorg/agent:tag", invocation.DockerAgent()),
		},
		{
			title: "🎉 You're all set",
			body: "Where to go from here:\n\n" +
				"  Ctrl+h            every key binding\n" +
				"  /getting-started  replay this tour\n\n" +
				"Something to tell us? Ctrl+k → \"Give Feedback\".",
		},
	}
}

// checkMessageSent matches the user sending a regular chat message (slash
// commands don't count; they have their own step).
func checkMessageSent(msg tea.Msg) bool {
	send, ok := msg.(messages.SendMsg)
	if !ok {
		return false
	}
	content := strings.TrimSpace(send.Content)
	return content != "" && !strings.HasPrefix(content, "/")
}

// checkToolCallApproved matches the user approving the pending tool call in
// the confirmation dialog, whatever the approval scope (once, always this
// tool, all tools). Rejections and tools that run without confirmation don't
// count: the step completes at the moment the user validates a tool call,
// not before.
func checkToolCallApproved(msg tea.Msg) bool {
	resume, ok := msg.(dialog.RuntimeResumeMsg)
	if !ok {
		return false
	}
	switch runtime.NormalizeResumeType(resume.Request.Type) {
	case runtime.ResumeTypeApprove, runtime.ResumeTypeApproveTool,
		runtime.ResumeTypeApproveBalanced, runtime.ResumeTypeApproveAutonomous:
		return true
	}
	return false
}

// checkPaletteOpened matches the command palette actually opening, which
// covers every way to reach it (Ctrl+k or a status-bar click).
func checkPaletteOpened(msg tea.Msg) bool {
	open, ok := msg.(dialog.OpenDialogMsg)
	return ok && dialog.IsCommandPalette(open.Model)
}

// checkSlashCommandUsed matches the messages produced by the slash commands
// the step suggests. The command palette can emit them too; either way the
// user learned a discoverable path to the feature.
func checkSlashCommandUsed(msg tea.Msg) bool {
	switch msg.(type) {
	case messages.OpenModelPickerMsg,
		messages.OpenSessionBrowserMsg,
		messages.OpenThemePickerMsg,
		messages.ShowToolsDialogMsg,
		messages.ShowCostDialogMsg:
		return true
	}
	return false
}
