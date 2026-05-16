package root

import (
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/acp"
	"github.com/docker/docker-agent/pkg/config"
	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/telemetry"
)

type acpFlags struct {
	runConfig config.RuntimeConfig
	sessionDB string
}

func newACPCmd() *cobra.Command {
	var flags acpFlags

	cmd := &cobra.Command{
		Use:   "acp <agent-file>|<registry-ref>",
		Short: "Start an agent as an ACP (Agent Client Protocol) server",
		Long:  "Start an ACP server that exposes the agent via the Agent Client Protocol",
		Example: `  docker-agent serve acp ./agent.yaml
  docker-agent serve acp ./team.yaml
  docker-agent serve acp agentcatalog/pirate`,
		Args: cobra.ExactArgs(1),
		RunE: flags.runACPCommand,
	}

	cmd.Flags().StringVarP(&flags.sessionDB, "session-db", "s", filepath.Join(paths.GetHomeDir(), ".cagent", "session.db"), "Path to the session database")
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func (f *acpFlags) runACPCommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "serve", append([]string{"acp"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(ctx, "serve", append([]string{"acp"}, args...), commandErr)
	}()

	agentFilename := args[0]

	// Expand tilde in session database path
	sessionDB, err := pathx.ExpandHomeDir(f.sessionDB)
	if err != nil {
		return err
	}

	return acp.Run(ctx, agentFilename, cmd.InOrStdin(), cmd.OutOrStdout(), &f.runConfig, sessionDB)
}
