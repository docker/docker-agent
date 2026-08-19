package root

import (
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mattn/go-runewidth"
	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	latestcfg "github.com/docker/docker-agent/pkg/config/latest"
	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func newAliasCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "alias",
		Short: "Manage aliases",
		Long:  "Create and manage aliases for agent configurations or OCI registry references.",
		Example: `  # Create an alias for a registry agent
  docker-agent alias add code myorg/notion-expert

  # Create an alias for a local agent file
  docker-agent alias add myagent ~/myagent.yaml

  # List all registered aliases
  docker-agent alias list

  # List all registered aliases as JSON
  docker-agent alias list --json

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
	safety          string
	model           string
	hideToolResults bool
	sandbox         bool
}

func newAliasAddCmd() *cobra.Command {
	var flags aliasAddFlags

	cmd := &cobra.Command{
		Use:   "add <alias-name> <agent-path>",
		Short: "Add a new alias",
		Long: `Add a new alias for an agent configuration or OCI registry reference.

You can optionally specify runtime options that will be applied whenever
the alias is used:

  --yolo               Automatically approve all tool calls without prompting
  --safety             Default safety mode: strict, balanced, restricted, or autonomous
  --model              Override the agent's model (format: [agent=]provider/model)
  --hide-tool-results  Hide tool call results in the TUI
  --sandbox            Always run the agent inside a Docker sandbox`,
		Example: `  # Create a simple alias
  docker-agent alias add code myorg/notion-expert

  # Create an alias that always runs in yolo mode
  docker-agent alias add yolo-coder myorg/coder --yolo

  # Create an alias that defaults to the balanced safety mode
  docker-agent alias add careful-coder myorg/coder --safety balanced

  # Create an alias with a specific model
  docker-agent alias add fast-coder myorg/coder --model openai/gpt-4o-mini

  # Create an alias with hidden tool results
  docker-agent alias add quiet myorg/coder --hide-tool-results

  # Create an alias that always runs in a sandbox
  docker-agent alias add safe-coder myorg/coder --sandbox

  # Create an alias with multiple options
  docker-agent alias add turbo myorg/coder --yolo --model anthropic/claude-sonnet-4-0`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAliasAddCommand(cmd, args, &flags)
		},
	}

	cmd.Flags().BoolVar(&flags.yolo, "yolo", false, "Automatically approve all tool calls without prompting")
	cmd.Flags().StringVar(&flags.safety, "safety", "", "Default safety mode when running the alias: strict, balanced, restricted, or autonomous (wins over --yolo)")
	cmd.Flags().StringVar(&flags.model, "model", "", "Override agent model (format: [agent=]provider/model)")
	cmd.Flags().BoolVar(&flags.hideToolResults, "hide-tool-results", false, "Hide tool call results in the TUI")
	cmd.Flags().BoolVar(&flags.sandbox, "sandbox", false, "Always run the agent inside a Docker sandbox")

	return cmd
}

func newAliasListCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List all registered aliases",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAliasListCommand(cmd, args, asJSON)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Output aliases as JSON")

	return cmd
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

	// Fail fast on a typo: only the canonical modes may be stored.
	safety := latestcfg.SafetyMode(flags.safety)
	if err := safety.Validate(); err != nil {
		return fmt.Errorf("invalid --safety value: %w", err)
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
		Safety:          safety,
		Model:           flags.model,
		HideToolResults: flags.hideToolResults,
		Sandbox:         flags.sandbox,
	}

	// Store the alias
	if err := userconfig.Update(func(cfg *userconfig.Config) error {
		return cfg.SetAlias(name, alias)
	}); err != nil {
		return err
	}

	out.Printf("Alias '%s' created successfully\n", name)
	out.Printf("  Alias: %s\n", name)
	out.Printf("  Agent: %s\n", absAgentPath)
	if flags.yolo {
		out.Printf("  Yolo:  enabled\n")
	}
	if safety != "" {
		out.Printf("  Safety: %s\n", string(safety))
	}
	if flags.model != "" {
		out.Printf("  Model: %s\n", flags.model)
	}
	if flags.hideToolResults {
		out.Printf("  Hide tool results: enabled\n")
	}
	if flags.sandbox {
		out.Printf("  Sandbox: enabled\n")
	}

	if name == "default" {
		out.Printf("\nYou can now run: docker agent run %s (or even docker agent run)\n", name)
	} else {
		out.Printf("\nYou can now run: docker agent run %s\n", name)
	}

	return nil
}

// aliasListEntry is the JSON representation of a single alias. It pairs the
// alias name (the map key in the config) with its embedded options.
type aliasListEntry struct {
	userconfig.Alias

	Name string `json:"name"`
}

func runAliasListCommand(cmd *cobra.Command, args []string, asJSON bool) (commandErr error) {
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

	// Sort aliases by name for consistent output
	names := slices.Sorted(maps.Keys(allAliases))

	if asJSON {
		entries := make([]aliasListEntry, 0, len(names))
		for _, name := range names {
			entries = append(entries, aliasListEntry{Name: name, Alias: *allAliases[name]})
		}
		encoded, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to encode aliases as JSON: %w", err)
		}
		out.Println(string(encoded))
		return nil
	}

	if len(allAliases) == 0 {
		out.Println("No aliases registered.")
		out.Println("\nCreate an alias with: docker agent alias add <name> <agent-path>")
		return nil
	}

	out.Printf("Registered aliases (%d):\n\n", len(allAliases))

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
		if alias.Safety != "" {
			options = append(options, "safety="+string(alias.Safety))
		}
		if alias.Model != "" {
			options = append(options, "model="+alias.Model)
		}
		if alias.HideToolResults {
			options = append(options, "hide-tool-results")
		}
		if alias.Sandbox {
			options = append(options, "sandbox")
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

	if err := userconfig.Update(func(cfg *userconfig.Config) error {
		if !cfg.DeleteAlias(name) {
			return fmt.Errorf("alias '%s' not found", name)
		}
		return nil
	}); err != nil {
		return err
	}

	out.Printf("Alias '%s' removed successfully\n", name)
	return nil
}
