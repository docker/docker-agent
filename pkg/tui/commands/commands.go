package commands

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/feedback"
	"github.com/docker/docker-agent/pkg/tools"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// ExecuteFunc is a function that executes a command with an optional argument.
type ExecuteFunc func(arg string) tea.Cmd

// Category represents a category of commands
type Category struct {
	Name     string
	Commands []Item
}

// ArgumentCandidate is one completable value for a command's argument,
// e.g. a toolset name for /toolset-restart. It is intentionally free of any
// completion-UI or app-domain types so pkg/tui/commands stays decoupled from
// both pkg/tui/components/completion and pkg/app.
type ArgumentCandidate struct {
	Label       string
	Description string
	// Disabled marks a candidate that is shown for context but cannot be
	// submitted (e.g. a non-restartable toolset for /toolset-restart).
	Disabled bool
}

// Item represents a single command in the palette
type Item struct {
	ID           string
	Label        string
	Description  string
	Category     string
	SlashCommand string
	Execute      ExecuteFunc
	Hidden       bool // Hidden commands work as slash commands but don't appear in the palette
	// Immediate marks commands that should run as soon as they are submitted
	// instead of being treated as ordinary queued chat input.
	Immediate bool
	// CompleteArgument, when set, returns the candidates for this command's
	// argument. Called lazily at completion-popup-open time so results
	// reflect current runtime state (e.g. toolset lifecycle). Nil for
	// commands with no argument completion.
	CompleteArgument func() []ArgumentCandidate
}

func builtInSessionCommands() []Item {
	cmds := []Item{
		{
			ID:           "session.clear",
			Label:        "Clear",
			SlashCommand: "/clear",
			Description:  "Clear the current tab and start a new session",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.ClearSessionMsg{})
			},
		},
		{
			ID:           "session.attach",
			Label:        "Attach",
			SlashCommand: "/attach",
			Description:  "Attach a file to your message (usage: /attach [path])",
			Category:     "Session",
			Immediate:    true,
			Execute: func(arg string) tea.Cmd {
				return core.CmdHandler(messages.AttachFileMsg{FilePath: arg})
			},
		},
		{
			ID:           "session.compact",
			Label:        "Compact",
			SlashCommand: "/compact",
			Description:  "Summarize the current conversation (usage: /compact [additional instructions])",
			Category:     "Session",
			Immediate:    true,
			Execute: func(arg string) tea.Cmd {
				return core.CmdHandler(messages.CompactSessionMsg{AdditionalPrompt: arg})
			},
		},
		{
			ID:           "session.clipboard",
			Label:        "Copy",
			SlashCommand: "/copy",
			Description:  "Copy the current conversation to the clipboard",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.CopySessionToClipboardMsg{})
			},
		},
		{
			ID:           "session.copy_last_response",
			Label:        "Copy Last Response",
			SlashCommand: "/copy-last",
			Description:  "Copy the last assistant message to the clipboard",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.CopyLastResponseToClipboardMsg{})
			},
		},
		{
			ID:           "session.undo",
			Label:        "Undo",
			SlashCommand: "/undo",
			Description:  "Restore file changes from the latest snapshot",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.UndoSnapshotMsg{})
			},
		},
		{
			ID:           "session.snapshots",
			Label:        "Snapshots",
			SlashCommand: "/snapshots",
			Description:  "List captured snapshots",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.ShowSnapshotsDialogMsg{})
			},
		},
		{
			ID:           "session.context",
			Label:        "Context",
			SlashCommand: "/context",
			Description:  "Show what is consuming the context window, by category",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.ShowContextDialogMsg{})
			},
		},
		{
			ID:           "session.drop",
			Label:        "Drop",
			SlashCommand: "/drop",
			Description:  "Remove an attached file from the session context (usage: /drop [path])",
			Category:     "Session",
			Immediate:    true,
			Execute: func(arg string) tea.Cmd {
				if arg = strings.TrimSpace(arg); arg != "" {
					return core.CmdHandler(messages.DropAttachedFileMsg{Path: arg})
				}
				// Without an argument, the /context dialog is the inventory
				// from which attachments can be reviewed and dropped.
				return core.CmdHandler(messages.ShowContextDialogMsg{})
			},
		},
		{
			ID:           "session.cost",
			Label:        "Cost",
			SlashCommand: "/cost",
			Description:  "Show detailed cost breakdown for this session",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.ShowCostDialogMsg{})
			},
		},
		{
			ID:           "session.effort",
			Label:        "Effort",
			SlashCommand: "/effort",
			Description:  "Set the reasoning effort of the current model (usage: /effort [level])",
			Category:     "Session",
			Immediate:    true,
			Execute: func(arg string) tea.Cmd {
				return core.CmdHandler(messages.SetThinkingLevelMsg{Level: strings.TrimSpace(arg)})
			},
		},
		{
			ID:           "session.eval",
			Label:        "Eval",
			SlashCommand: "/eval",
			Description:  "Create an evaluation report (usage: /eval [filename])",
			Category:     "Session",
			Immediate:    true,
			Execute: func(arg string) tea.Cmd {
				return core.CmdHandler(messages.EvalSessionMsg{Filename: arg})
			},
		},
		{
			ID:           "session.fork",
			Label:        "Fork",
			SlashCommand: "/fork",
			Description:  "Fork the current session into a new tab",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.ForkSessionMsg{})
			},
		},
		{
			ID:           "session.exit",
			Label:        "Exit",
			SlashCommand: "/exit",
			Description:  "Exit the application",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.ExitSessionMsg{})
			},
		},
		{
			ID:           "session.quit",
			Label:        "Quit",
			SlashCommand: "/quit",
			Description:  "Quit the application (alias for /exit)",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.ExitSessionMsg{})
			},
		},
		{
			ID:           "session.q",
			Label:        "Quit",
			SlashCommand: "/q",
			Hidden:       true,
			Description:  "Quit the application (alias for /exit)",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.ExitSessionMsg{})
			},
		},
		{
			ID:           "session.export",
			Label:        "Export",
			SlashCommand: "/export",
			Description:  "Export the session as HTML (usage: /export [filename])",
			Category:     "Session",
			Immediate:    true,
			Execute: func(arg string) tea.Cmd {
				return core.CmdHandler(messages.ExportSessionMsg{Filename: arg})
			},
		},
		{
			ID:           "session.model",
			Label:        "Model",
			SlashCommand: "/model",
			Description:  "Change the model for the current agent",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.OpenModelPickerMsg{})
			},
		},
		{
			ID:           "session.new",
			Label:        "New",
			SlashCommand: "/new",
			Description:  "Start a new conversation (usage: /new [dir])",
			Category:     "Session",
			Immediate:    true,
			Execute: func(arg string) tea.Cmd {
				return core.CmdHandler(messages.NewSessionMsg{WorkingDir: strings.TrimSpace(arg)})
			},
		},
		{
			ID:           "session.pause",
			Label:        "Pause",
			SlashCommand: "/pause",
			Description:  "Pause/resume the runtime loop after the current request",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.TogglePauseMsg{})
			},
		},
		{
			ID:           "session.permissions",
			Label:        "Permissions",
			SlashCommand: "/permissions",
			Description:  "Show tool permission rules for this session",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.ShowPermissionsDialogMsg{})
			},
		},
		{
			ID:           "session.plans",
			Label:        "Plans",
			SlashCommand: "/plans",
			Description:  "Browse and manage shared plans and this session's plan",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.ShowPlanBrowserMsg{})
			},
		},
		{
			ID:           "session.history",
			Label:        "Sessions",
			SlashCommand: "/sessions",
			Description:  "Browse and load past sessions",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.OpenSessionBrowserMsg{})
			},
		},
		{
			ID:           "session.shell",
			Label:        "Shell",
			SlashCommand: "/shell",
			Description:  "Start a shell",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.StartShellMsg{})
			},
		},
		{
			ID:           "session.star",
			Label:        "Star",
			SlashCommand: "/star",
			Description:  "Toggle star on current session",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.ToggleSessionStarMsg{})
			},
		},

		{
			ID:           "session.tools",
			Label:        "Tools",
			SlashCommand: "/tools",
			Description:  "Show every toolset (with lifecycle state) and the tools they expose",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.ShowToolsDialogMsg{})
			},
		},
		{
			ID:           "session.skills",
			Label:        "Skills",
			SlashCommand: "/skills",
			Description:  "List skills available to the current agent",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.ShowSkillsDialogMsg{})
			},
		},
		{
			ID:           "session.toolset.restart",
			Label:        "Restart Toolset",
			SlashCommand: "/toolset-restart",
			Description:  "Force a supervisor-driven restart of one toolset (usage: /toolset-restart <name>)",
			Category:     "Session",
			Immediate:    true,
			Execute: func(arg string) tea.Cmd {
				name := strings.TrimSpace(arg)
				return core.CmdHandler(messages.RestartToolsetMsg{Name: name})
			},
		},
		{
			ID:           "session.title",
			Label:        "Title",
			SlashCommand: "/title",
			Description:  "Set or regenerate session title (usage: /title [new title])",
			Category:     "Session",
			Immediate:    true,
			Execute: func(arg string) tea.Cmd {
				arg = strings.TrimSpace(arg)
				if arg == "" {
					// No argument: regenerate title
					return core.CmdHandler(messages.RegenerateTitleMsg{})
				}
				// With argument: set title
				return core.CmdHandler(messages.SetSessionTitleMsg{Title: arg})
			},
		},
		{
			ID:           "session.yolo",
			Label:        "Yolo",
			SlashCommand: "/yolo",
			Description:  "Toggle automatic approval of tool calls",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.ToggleYoloMsg{})
			},
		},
	}

	// Add speak command on supported platforms (macOS only)
	if speak := speakCommand(); speak != nil {
		cmds = append(cmds, *speak)
	}

	return cmds
}

func builtInSettingsCommands() []Item {
	return []Item{
		{
			ID:           "settings.open",
			Label:        "Settings",
			SlashCommand: "/settings",
			Description:  "Manage appearance, behavior, and notification preferences",
			Category:     "Settings",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.OpenSettingsDialogMsg{})
			},
		},
	}
}

func builtInHelpCommands() []Item {
	return []Item{
		{
			ID:           "help.getting-started",
			Label:        "Getting Started Tour",
			SlashCommand: "/getting-started",
			Description:  "Learn docker agent by doing, a 2-minute interactive tour",
			Category:     "Help",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.StartTourMsg{})
			},
		},
	}
}

func builtInFeedbackCommands() []Item {
	return []Item{
		{
			ID:          "feedback.feedback",
			Label:       "Give Feedback",
			Description: "Provide feedback about docker agent",
			Category:    "Feedback",
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.OpenURLMsg{URL: feedback.Link})
			},
		},
		{
			ID:          "feedback.bug",
			Label:       "Report Bug",
			Description: "Report a bug or issue",
			Category:    "Feedback",
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.OpenURLMsg{URL: "https://github.com/docker/docker-agent/issues/new/choose"})
			},
		},
	}
}

// visibleOnly returns items that are not hidden.
func visibleOnly(items []Item) []Item {
	visible := make([]Item, 0, len(items))
	for _, item := range items {
		if !item.Hidden {
			visible = append(visible, item)
		}
	}
	return visible
}

// sortByLabel returns items sorted alphabetically by label.
func sortByLabel(items []Item) []Item {
	slices.SortFunc(items, func(a, b Item) int {
		return strings.Compare(strings.ToLower(a.Label), strings.ToLower(b.Label))
	})
	return items
}

// snapshotCommandIDs is the set of IDs that depend on the snapshot feature.
// They are stripped from the palette and the slash-command parser when
// snapshots are turned off.
var snapshotCommandIDs = map[string]bool{
	"session.undo":      true,
	"session.snapshots": true,
}

// removeByIDs returns items whose IDs are not in ids.
func removeByIDs(items []Item, ids map[string]bool) []Item {
	out := make([]Item, 0, len(items))
	for _, item := range items {
		if !ids[item.ID] {
			out = append(out, item)
		}
	}
	return out
}

// newAgentCommandItem builds the command-palette / slash-command Item for a
// single agent command. SlashCommand and Immediate are set so the command
// dispatches from the keyboard (not only from the palette); Execute forwards
// the slash command, with any argument appended, to the agent as an
// AgentCommandMsg.
func newAgentCommandItem(commandName string, cmd types.Command) Item {
	return Item{
		ID:           "agent.command." + commandName,
		Label:        commandName,
		Description:  toolcommon.TruncateText(cmd.DisplayText(), 60),
		Category:     "Agent Commands",
		SlashCommand: "/" + commandName,
		Immediate:    true,
		Execute: func(arg string) tea.Cmd {
			input := "/" + commandName
			if arg = strings.TrimSpace(arg); arg != "" {
				input += " " + arg
			}
			return core.CmdHandler(messages.AgentCommandMsg{Command: input})
		},
	}
}

// newMCPPromptItem builds the command-palette / slash-command Item for a single
// MCP prompt. SlashCommand and Immediate are set so the prompt dispatches from
// the keyboard (not only from the palette dialog); Execute maps a non-empty
// argument string to the prompt's first declared argument, and otherwise runs
// with empty arguments (no required args) or opens the input dialog (a required
// argument is missing).
func newMCPPromptItem(promptName string, promptInfo mcptools.PromptInfo) Item {
	// Build description with argument info
	description := promptInfo.Description
	if len(promptInfo.Arguments) > 0 {
		// Count required arguments
		requiredCount := 0
		for _, arg := range promptInfo.Arguments {
			if arg.Required {
				requiredCount++
			}
		}

		if requiredCount > 0 {
			if description != "" {
				description += " "
			}
			if requiredCount == 1 {
				description += "(1 required arg)"
			} else {
				description += fmt.Sprintf("(%d required args)", requiredCount)
			}
		}
	}

	// Truncate long descriptions to fit on one line
	description = toolcommon.TruncateText(description, 55)

	return Item{
		ID:           "mcp.prompt." + promptName,
		Label:        promptName,
		Description:  description,
		Category:     "MCP Prompts",
		SlashCommand: "/" + promptName,
		Immediate:    true,
		Execute: func(arg string) tea.Cmd {
			arg = strings.TrimSpace(arg)

			// Slash command with argument: map to the first declared prompt argument.
			if arg != "" && len(promptInfo.Arguments) > 0 {
				return core.CmdHandler(messages.MCPPromptMsg{
					PromptName: promptName,
					Arguments:  map[string]string{promptInfo.Arguments[0].Name: arg},
				})
			}

			// No arg provided (palette click or slash with no arg): original behavior.
			hasRequiredArgs := false
			for _, a := range promptInfo.Arguments {
				if a.Required {
					hasRequiredArgs = true
					break
				}
			}

			if !hasRequiredArgs {
				// Execute prompt with empty arguments
				return core.CmdHandler(messages.MCPPromptMsg{
					PromptName: promptName,
					Arguments:  make(map[string]string),
				})
			}
			// Show parameter input dialog for prompts with required arguments
			return core.CmdHandler(messages.ShowMCPPromptInputMsg{
				PromptName: promptName,
				PromptInfo: promptInfo,
			})
		},
	}
}

// toolsetStatusSource is the minimal surface toolsetRestartCandidates needs.
// *app.App satisfies it; tests can supply a stub instead of constructing a
// full application.
type toolsetStatusSource interface {
	CurrentAgentToolsetStatuses() []tools.ToolsetStatus
}

// toolsetRestartCandidates returns one ArgumentCandidate per toolset of the
// current agent, in declaration order, deduplicated by name (preferring the
// restartable entry when duplicate names disagree — display names are not
// guaranteed unique, mirroring runtime.RestartToolset's own matching).
// Non-restartable toolsets are marked Disabled rather than omitted, so the
// popup teaches users the full toolset inventory and why a restart isn't
// offered for some of them.
func toolsetRestartCandidates(source toolsetStatusSource) []ArgumentCandidate {
	statuses := source.CurrentAgentToolsetStatuses()
	byName := make(map[string]tools.ToolsetStatus, len(statuses))
	order := make([]string, 0, len(statuses))
	for _, s := range statuses {
		if s.Name == "" {
			continue
		}
		if existing, ok := byName[s.Name]; !ok {
			byName[s.Name] = s
			order = append(order, s.Name)
		} else if !existing.Restartable && s.Restartable {
			byName[s.Name] = s
		}
	}

	candidates := make([]ArgumentCandidate, 0, len(order))
	for _, name := range order {
		s := byName[name]
		candidates = append(candidates, ArgumentCandidate{
			Label:       s.Name,
			Description: cmp.Or(s.Kind, "Built-in") + " · " + s.State.String(),
			Disabled:    !s.Restartable,
		})
	}
	return candidates
}

// attachToolsetRestartCompletion wires the toolset-name argument completer
// onto the /toolset-restart item. Attached post-hoc (rather than inline in
// builtInSessionCommands) so that function stays free of any status-source
// dependency.
func attachToolsetRestartCompletion(items []Item, source toolsetStatusSource) {
	for i := range items {
		if items[i].ID != "session.toolset.restart" {
			continue
		}
		items[i].CompleteArgument = func() []ArgumentCandidate {
			return toolsetRestartCandidates(source)
		}
		return
	}
}

// effortLevelsSource is the minimal surface effortCandidates needs. *app.App
// satisfies it; tests can supply a stub instead of constructing a full
// application.
type effortLevelsSource interface {
	CurrentAgentThinkingLevels(ctx context.Context) []effort.Level
}

// effortCandidates returns one ArgumentCandidate per thinking-effort level
// the current agent's active model supports, in the source's canonical
// order. Unlike toolsetRestartCandidates, unsupported levels are never
// listed (let alone Disabled): SetAgentThinkingLevel hard-rejects any level
// outside this set, so offering it would complete-then-fail. A source with
// no resolvable levels (remote runtime, non-reasoning model) yields no
// candidates, and the popup simply doesn't open.
func effortCandidates(ctx context.Context, source effortLevelsSource) []ArgumentCandidate {
	levels := source.CurrentAgentThinkingLevels(ctx)
	candidates := make([]ArgumentCandidate, 0, len(levels))
	for _, level := range levels {
		candidates = append(candidates, ArgumentCandidate{Label: string(level)})
	}
	return candidates
}

// attachEffortCompletion wires the effort-level argument completer onto the
// /effort item. Attached post-hoc (rather than inline in
// builtInSessionCommands) so that function stays free of any
// effort-levels-source dependency. ctx is the long-lived TUI context, closed
// over by the completer so it can re-resolve the model on every call.
func attachEffortCompletion(items []Item, ctx context.Context, source effortLevelsSource) {
	for i := range items {
		if items[i].ID != "session.effort" {
			continue
		}
		items[i].CompleteArgument = func() []ArgumentCandidate {
			return effortCandidates(ctx, source)
		}
		return
	}
}

// attachedFilesSource is the minimal surface dropCandidates needs. *app.App
// satisfies it; tests can supply a stub instead of constructing a full
// application.
type attachedFilesSource interface {
	AttachedFiles() []string
}

// dropCandidates returns one ArgumentCandidate per file currently attached to
// the session, in attachment order. Labels are the exact recorded (absolute)
// paths: completing them hits the exact-match branch of resolveAttachedFile,
// and paths containing spaces stay safe because the completion Value carries
// the full command line, not just the label. Every attached file is
// droppable, so Disabled never applies here.
func dropCandidates(source attachedFilesSource) []ArgumentCandidate {
	attached := source.AttachedFiles()
	candidates := make([]ArgumentCandidate, 0, len(attached))
	for _, path := range attached {
		candidates = append(candidates, ArgumentCandidate{Label: path})
	}
	return candidates
}

// attachDropCompletion wires the attached-file argument completer onto the
// /drop item. Attached post-hoc (rather than inline in
// builtInSessionCommands) so that function stays free of any
// attached-files-source dependency.
func attachDropCompletion(items []Item, source attachedFilesSource) {
	for i := range items {
		if items[i].ID != "session.drop" {
			continue
		}
		items[i].CompleteArgument = func() []ArgumentCandidate {
			return dropCandidates(source)
		}
		return
	}
}

// BuildCommandCategories builds the list of command categories for the command palette
func BuildCommandCategories(ctx context.Context, application *app.App) []Category {
	// Get session commands and filter based on model capabilities
	sessionCommands := builtInSessionCommands()
	if !application.SnapshotsEnabled() {
		sessionCommands = removeByIDs(sessionCommands, snapshotCommandIDs)
	}
	attachToolsetRestartCompletion(sessionCommands, application)
	attachEffortCompletion(sessionCommands, ctx, application)
	attachDropCompletion(sessionCommands, application)

	categories := []Category{
		{
			Name:     "Session",
			Commands: sessionCommands,
		},
	}

	agentCommands := application.CurrentAgentCommands(ctx)
	if len(agentCommands) > 0 {
		commands := make([]Item, 0, len(agentCommands))
		for name, cmd := range agentCommands {
			commands = append(commands, newAgentCommandItem(name, cmd))
		}

		categories = append(categories, Category{
			Name:     "Agent Commands",
			Commands: commands,
		})
	}

	mcpPrompts := application.CurrentMCPPrompts(ctx)
	if len(mcpPrompts) > 0 {
		mcpCommands := make([]Item, 0, len(mcpPrompts))
		for promptName, promptInfo := range mcpPrompts {
			mcpCommands = append(mcpCommands, newMCPPromptItem(promptName, promptInfo))
		}

		categories = append(categories, Category{
			Name:     "MCP Prompts",
			Commands: mcpCommands,
		})
	}

	// Add skill commands if skills are enabled for the current agent
	skillsList := application.CurrentAgentSkills()
	if len(skillsList) > 0 {
		skillCommands := make([]Item, 0, len(skillsList))
		for _, skill := range skillsList {
			skillName := skill.Name
			description := toolcommon.TruncateText(skill.Description, 55)

			skillCommands = append(skillCommands, Item{
				ID:           "skill." + skillName,
				Label:        skillName,
				Description:  description,
				Category:     "Skills",
				SlashCommand: "/" + skillName,
				Immediate:    true,
				Execute: func(arg string) tea.Cmd {
					input := "/" + skillName
					if arg = strings.TrimSpace(arg); arg != "" {
						input += " " + arg
					}
					return core.CmdHandler(messages.SendMsg{Content: input, BypassQueue: true})
				},
			})
		}

		categories = append(categories, Category{
			Name:     "Skills",
			Commands: skillCommands,
		})
	}

	// Settings, Help, and Feedback are always last, in that order.
	categories = append(categories,
		Category{
			Name:     "Settings",
			Commands: builtInSettingsCommands(),
		},
		Category{
			Name:     "Help",
			Commands: builtInHelpCommands(),
		},
		Category{
			Name:     "Feedback",
			Commands: builtInFeedbackCommands(),
		},
	)

	// Filter out hidden commands and sort by label in all categories.
	for i := range categories {
		categories[i].Commands = sortByLabel(visibleOnly(categories[i].Commands))
	}

	return categories
}

type Parser struct {
	categories []Category
}

func NewParser(categories ...Category) *Parser {
	return &Parser{
		categories: categories,
	}
}

func (p *Parser) Parse(input string) tea.Cmd {
	if input == "" || input[0] != '/' {
		return nil
	}

	// Split into command and argument
	cmd, arg, _ := strings.Cut(input, " ")

	// Search through all categories and commands
	for _, category := range p.categories {
		for _, item := range category.Commands {
			if item.SlashCommand == cmd && item.Immediate {
				return item.Execute(arg)
			}
		}
	}

	return nil
}
