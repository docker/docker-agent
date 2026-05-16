package commands

import (
	"context"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/feedback"
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

// Item represents a single command in the palette
type Item struct {
	ID           string
	Label        string
	Description  string
	Category     string
	SlashCommand string
	Execute      ExecuteFunc
	Hidden       bool // Hidden commands work as slash commands but don't appear in the palette
	// Immediate marks that the command can be executed immediately as it does not
	// interrupt any ongoing stream.
	Immediate bool
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
			Description:  "Start a new conversation",
			Category:     "Session",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.NewSessionMsg{})
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
			ID:           "settings.split-diff",
			Label:        "Split Diff",
			SlashCommand: "/split-diff",
			Description:  "Toggle split diff view mode",
			Category:     "Settings",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.ToggleSplitDiffMsg{})
			},
		},
		{
			ID:           "settings.theme",
			Label:        "Theme",
			SlashCommand: "/theme",
			Description:  "Change the color theme",
			Category:     "Settings",
			Immediate:    true,
			Execute: func(string) tea.Cmd {
				return core.CmdHandler(messages.OpenThemePickerMsg{})
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

// BuildCommandCategories builds the list of command categories for the command palette
func BuildCommandCategories(ctx context.Context, application *app.App) []Category {
	// Get session commands and filter based on model capabilities
	sessionCommands := builtInSessionCommands()
	if !application.SnapshotsEnabled() {
		sessionCommands = removeByIDs(sessionCommands, snapshotCommandIDs)
	}

	categories := []Category{
		{
			Name:     "Session",
			Commands: sessionCommands,
		},
	}

	agentCommands := application.CurrentAgentCommands(ctx)
	if len(agentCommands) > 0 {
		var commands []Item
		for name, cmd := range agentCommands {
			commands = append(commands, Item{
				ID:           "agent.command." + name,
				Label:        name,
				Description:  toolcommon.TruncateText(cmd.DisplayText(), 60),
				Category:     "Agent Commands",
				SlashCommand: "/" + name,
				Execute: func(string) tea.Cmd {
					return core.CmdHandler(messages.AgentCommandMsg{Command: "/" + name})
				},
			})
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

			// Create closure variables to capture current iteration values
			currentPromptName := promptName
			currentPromptInfo := promptInfo

			mcpCommands = append(mcpCommands, Item{
				ID:          "mcp.prompt." + promptName,
				Label:       promptName,
				Description: description,
				Category:    "MCP Prompts",
				Execute: func(string) tea.Cmd {
					// If prompt has no required arguments, execute immediately
					hasRequiredArgs := false
					for _, arg := range currentPromptInfo.Arguments {
						if arg.Required {
							hasRequiredArgs = true
							break
						}
					}

					if !hasRequiredArgs {
						// Execute prompt with empty arguments
						return core.CmdHandler(messages.MCPPromptMsg{
							PromptName: currentPromptName,
							Arguments:  make(map[string]string),
						})
					} else {
						// Show parameter input dialog for prompts with required arguments
						return core.CmdHandler(messages.ShowMCPPromptInputMsg{
							PromptName: currentPromptName,
							PromptInfo: currentPromptInfo,
						})
					}
				},
			})
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
				Execute: func(arg string) tea.Cmd {
					input := "/" + skillName
					if arg = strings.TrimSpace(arg); arg != "" {
						input += " " + arg
					}
					return core.CmdHandler(messages.SendMsg{Content: input})
				},
			})
		}

		categories = append(categories, Category{
			Name:     "Skills",
			Commands: skillCommands,
		})
	}

	// Settings and Feedback are always last, in that order.
	categories = append(categories,
		Category{
			Name:     "Settings",
			Commands: builtInSettingsCommands(),
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
