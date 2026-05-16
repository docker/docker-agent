package root

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/goccy/go-yaml"
	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/telemetry"
)

type debugFlags struct {
	modelOverrides []string
	runConfig      config.RuntimeConfig
}

func newDebugCmd() *cobra.Command {
	var flags debugFlags

	cmd := &cobra.Command{
		Use:     "debug",
		Short:   "Debug tools",
		GroupID: "advanced",
	}
	cmd.Hidden = true

	cmd.AddCommand(&cobra.Command{
		Use:   "config <agent-file>|<registry-ref>",
		Short: "Print the canonical form of an agent's configuration file",
		Args:  cobra.ExactArgs(1),
		RunE:  flags.runDebugConfigCommand,
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "toolsets <agent-file>|<registry-ref>",
		Short: "Debug the toolsets of an agent",
		Args:  cobra.ExactArgs(1),
		RunE:  flags.runDebugToolsetsCommand,
	})
	titleCmd := &cobra.Command{
		Use:   "title <agent-file>|<registry-ref> <question>",
		Short: "Generate a session title from a question",
		Args:  cobra.ExactArgs(2),
		RunE:  flags.runDebugTitleCommand,
	}
	titleCmd.Flags().StringArrayVar(&flags.modelOverrides, "model", nil, "Override agent model: [agent=]provider/model (repeatable)")
	cmd.AddCommand(titleCmd)

	addRuntimeConfigFlags(cmd, &flags.runConfig)

	cmd.AddCommand(newDebugAuthCmd())
	cmd.AddCommand(newDebugOAuthCmd())

	return cmd
}

// loadTeam loads an agent team from the given agent file.
// Callers should defer stopToolSets(t) to clean up.
func (f *debugFlags) loadTeam(ctx context.Context, agentFilename string, opts ...teamloader.Opt) (*team.Team, error) {
	agentSource, err := config.Resolve(agentFilename, f.runConfig.EnvProvider())
	if err != nil {
		return nil, err
	}

	t, err := teamloader.Load(ctx, agentSource, &f.runConfig, opts...)
	if err != nil {
		return nil, err
	}

	return t, nil
}

func (f *debugFlags) runDebugConfigCommand(cmd *cobra.Command, args []string) (commandErr error) {
	telemetry.TrackCommand(cmd.Context(), "debug", append([]string{"config"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(cmd.Context(), "debug", append([]string{"config"}, args...), commandErr)
	}()

	agentSource, err := config.Resolve(args[0], f.runConfig.EnvProvider())
	if err != nil {
		return err
	}

	cfg, err := config.Load(cmd.Context(), agentSource)
	if err != nil {
		return err
	}

	return yaml.NewEncoder(cmd.OutOrStdout()).Encode(cfg)
}

func (f *debugFlags) runDebugToolsetsCommand(cmd *cobra.Command, args []string) (commandErr error) {
	telemetry.TrackCommand(cmd.Context(), "debug", append([]string{"toolsets"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(cmd.Context(), "debug", append([]string{"toolsets"}, args...), commandErr)
	}()

	ctx := cmd.Context()

	t, err := f.loadTeam(ctx, args[0])
	if err != nil {
		return err
	}
	defer stopToolSets(t)

	out := cli.NewPrinter(cmd.OutOrStdout())

	for _, name := range t.AgentNames() {
		agent, err := t.Agent(name)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to get agent", "name", name, "error", err)
			continue
		}

		tools, err := agent.Tools(ctx)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to query tools", "name", agent.Name(), "error", err)
			continue
		}

		if len(tools) == 0 {
			out.Printf("No tools for %s\n", agent.Name())
			continue
		}

		out.Printf("%d tool(s) for %s:\n", len(tools), agent.Name())
		for _, tool := range tools {
			out.Println(" +", tool.Name, "-", tool.Description)
		}
	}

	return nil
}

func (f *debugFlags) runDebugTitleCommand(cmd *cobra.Command, args []string) (commandErr error) {
	telemetry.TrackCommand(cmd.Context(), "debug", append([]string{"title"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(cmd.Context(), "debug", append([]string{"title"}, args...), commandErr)
	}()

	ctx := cmd.Context()

	t, err := f.loadTeam(ctx, args[0], teamloader.WithModelOverrides(f.modelOverrides))
	if err != nil {
		return err
	}
	defer stopToolSets(t)

	agent, err := t.DefaultAgent()
	if err != nil {
		return err
	}

	model := agent.Model(ctx)
	if model == nil {
		return fmt.Errorf("agent %q has no model configured", agent.Name())
	}

	// Use the same title generation code path as the TUI (see runTUI in new.go)
	gen := sessiontitle.New(model, agent.FallbackModels()...)

	title, err := gen.Generate(ctx, "debug", []string{args[1]})
	if err != nil {
		return fmt.Errorf("generating title: %w", err)
	}

	fmt.Fprintln(cmd.OutOrStdout(), title)

	return nil
}
