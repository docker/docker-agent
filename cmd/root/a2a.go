package root

import (
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/a2a"
	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/telemetry"
)

type a2aFlags struct {
	agentName  string
	listenAddr string
	sessionDB  string
	runConfig  config.RuntimeConfig
}

func newA2ACmd() *cobra.Command {
	var flags a2aFlags

	cmd := &cobra.Command{
		Use:   "a2a <agent-file>|<registry-ref>",
		Short: "Start an agent as an A2A (Agent-to-Agent) server",
		Long:  "Start an A2A server that exposes the agent via the Agent-to-Agent protocol",
		Example: `  docker-agent serve a2a ./agent.yaml
  docker-agent serve a2a agentcatalog/pirate --listen 127.0.0.1:9090`,
		Args: cobra.ExactArgs(1),
		RunE: flags.runA2ACommand,
	}

	cmd.PersistentFlags().StringVarP(&flags.agentName, "agent", "a", "", "Name of the agent to run (defaults to the team's first agent)")
	cmd.PersistentFlags().StringVarP(&flags.listenAddr, "listen", "l", "127.0.0.1:8082", "Address to listen on")
	cmd.PersistentFlags().StringVarP(&flags.sessionDB, "session-db", "s", filepath.Join(paths.GetHomeDir(), ".cagent", "session.db"), "Path to the session database")
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func (f *a2aFlags) runA2ACommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "serve", append([]string{"a2a"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(ctx, "serve", append([]string{"a2a"}, args...), commandErr)
	}()

	out := cli.NewPrinter(cmd.OutOrStdout())
	agentFilename := args[0]

	ln, cleanup, err := newListener(ctx, f.listenAddr)
	if err != nil {
		return err
	}
	defer cleanup()

	out.Println("Listening on", ln.Addr().String())
	return a2a.Run(ctx, agentFilename, f.agentName, f.sessionDB, &f.runConfig, ln)
}
