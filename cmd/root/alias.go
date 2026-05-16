package root

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mattn/go-runewidth"
	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func newAliasCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "alias",
		Short: "Manage aliases",
		Long:  "Create and manage aliases for agent configurations or catalog references.",
		Example: `  # Create an alias for a catalog agent
  docker-agent alias add code agentcatalog/notion-expert

  # Create an alias for a local agent file
  docker-agent alias add myagent ~/myagent.yaml

  # List all registered aliases
  docker-agent alias list

  # Remove an alias
  docker-agent alias remove code`,
		GroupID: "advanced",
	}

	cmd.AddCommand(newAliasAddCmd())
	cmd.AddCommand(newAliasListCmd())
	cmd.AddCommand(newAliasRemoveCmd())

	return cmd
}

type aliasAddFlags struct {
	yolo            bool
	model           string
	hideToolResults bool
}

func newAliasAddCmd() *cobra.Command {
	var flags aliasAddFlags

	cmd := &cobra.Command{
		Use:   "add <alias-name> <agent-path>",
		Short: "Add a new alias",
		Long: `Add a new alias for an agent configuration or catalog reference.

You can optionally specify runtime options that will be applied whenever
the alias is used:

  --yolo               Automatically approve all tool calls without prompting
  --model              Override the agent's model (format: [agent=]provider/model)
  --hide-tool-results  Hide tool call results in the TUI`,
		Example: `  # Create a simple alias
  docker-agent alias add code agentcatalog/notion-expert

  # Create an alias that always runs in yolo mode
  docker-agent alias add yolo-coder agentcatalog/coder --yolo

  # Create an alias with a specific model
  docker-agent alias add fast-coder agentcatalog/coder --model openai/gpt-4o-mini

  # Create an alias with hidden tool results
  docker-agent alias add quiet agentcatalog/coder --hide-tool-results

  # Create an alias with multiple options
  docker-agent alias add turbo agentcatalog/coder --yolo --model anthropic/claude-sonnet-4-0`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAliasAddCommand(cmd, args, &flags)
		},
	}

	cmd.Flags().BoolVar(&flags.yolo, "yolo", false, "Automatically approve all tool calls without prompting")
	cmd.Flags().StringVar(&flags.model, "model", "", "Override agent model (format: [agent=]provider/model)")
	cmd.Flags().BoolVar(&flags.hideToolResults, "hide-tool-results", false, "Hide tool call results in the TUI")

	return cmd
}

func newAliasListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List all registered aliases",
		Args:    cobra.NoArgs,
		RunE:    runAliasListCommand,
	}
}

func newAliasRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <alias-name>",
		Aliases: []string{"rm"},
		Short:   "Remove a registered alias",
		Args:    cobra.ExactArgs(1),
		RunE:    runAliasRemoveCommand,
	}
}

func runAliasAddCommand(cmd *cobra.Command, args []string, flags *aliasAddFlags) (commandErr error) {
	telemetry.TrackCommand(cmd.Context(), "alias", append([]string{"add"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(cmd.Context(), "alias", append([]string{"add"}, args...), commandErr)
	}()

	out := cli.NewPrinter(cmd.OutOrStdout())
	name := args[0]
	agentPath := args[1]

	cfg, err := userconfig.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	absAgentPath, err := pathx.ExpandHomeDir(agentPath)
	if err != nil {
		return err
	}

	// Convert relative paths to absolute for local files (not OCI references or URLs)
	if !config.IsOCIReference(absAgentPath) && !config.IsURLReference(absAgentPath) && !filepath.IsAbs(absAgentPath) {
		absAgentPath, err = filepath.Abs(absAgentPath)
		if err != nil {
			return fmt.Errorf("failed to resolve absolute path: %w", err)
		}
	}

	// Create alias with options
	alias := &userconfig.Alias{
		Path:            absAgentPath,
		Yolo:            flags.yolo,
		Model:           flags.model,
		HideToolResults: flags.hideToolResults,
	}

	// Store the alias
	if err := cfg.SetAlias(name, alias); err != nil {
		return err
	}

	// Save to file
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	out.Printf("Alias '%s' created successfully\n", name)
	out.Printf("  Alias: %s\n", name)
	out.Printf("  Agent: %s\n", absAgentPath)
	if flags.yolo {
		out.Printf("  Yolo:  enabled\n")
	}
	if flags.model != "" {
		out.Printf("  Model: %s\n", flags.model)
	}
	if flags.hideToolResults {
		out.Printf("  Hide tool results: enabled\n")
	}

	if name == "default" {
		out.Printf("\nYou can now run: docker agent run %s (or even docker agent run)\n", name)
	} else {
		out.Printf("\nYou can now run: docker agent run %s\n", name)
	}

	return nil
}

func runAliasListCommand(cmd *cobra.Command, args []string) (commandErr error) {
	telemetry.TrackCommand(cmd.Context(), "alias", append([]string{"list"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(cmd.Context(), "alias", append([]string{"list"}, args...), commandErr)
	}()

	out := cli.NewPrinter(cmd.OutOrStdout())

	cfg, err := userconfig.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	allAliases := cfg.Aliases
	if len(allAliases) == 0 {
		out.Println("No aliases registered.")
		out.Println("\nCreate an alias with: docker agent alias add <name> <agent-path>")
		return nil
	}

	out.Printf("Registered aliases (%d):\n\n", len(allAliases))

	// Sort aliases by name for consistent output
	names := slices.Sorted(maps.Keys(allAliases))

	// Find max name width for alignment (using display width for proper Unicode handling)
	maxLen := 0
	for _, name := range names {
		maxLen = max(maxLen, runewidth.StringWidth(name))
	}

	for _, name := range names {
		alias := allAliases[name]
		padding := strings.Repeat(" ", maxLen-runewidth.StringWidth(name))

		// Build options string
		var options []string
		if alias.Yolo {
			options = append(options, "yolo")
		}
		if alias.Model != "" {
			options = append(options, "model="+alias.Model)
		}
		if alias.HideToolResults {
			options = append(options, "hide-tool-results")
		}

		if len(options) > 0 {
			out.Printf("  %s%s → %s [%s]\n", name, padding, alias.Path, strings.Join(options, ", "))
		} else {
			out.Printf("  %s%s → %s\n", name, padding, alias.Path)
		}
	}

	out.Println("\nRun an alias with: docker agent run <alias>")

	return nil
}

func runAliasRemoveCommand(cmd *cobra.Command, args []string) (commandErr error) {
	telemetry.TrackCommand(cmd.Context(), "alias", append([]string{"remove"}, args...))
	defer func() {
		telemetry.TrackCommandError(cmd.Context(), "alias", append([]string{"remove"}, args...), commandErr)
	}()

	out := cli.NewPrinter(cmd.OutOrStdout())
	name := args[0]

	cfg, err := userconfig.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if !cfg.DeleteAlias(name) {
		return fmt.Errorf("alias '%s' not found", name)
	}

	if err := cfg.Save(); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	out.Printf("Alias '%s' removed successfully\n", name)
	return nil
}
