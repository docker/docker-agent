package root

import (
	"context"
	"errors"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/mcp"
	"github.com/docker/docker-agent/pkg/runregistry"
	"github.com/docker/docker-agent/pkg/telemetry"
)

type mcpFlags struct {
	agentName  string
	http       bool
	listenAddr string
	attach     string
	runConfig  config.RuntimeConfig
}

func newMCPCmd() *cobra.Command {
	var flags mcpFlags

	cmd := &cobra.Command{
		Use:   "mcp <agent-file>|<registry-ref>",
		Short: "Start an agent as an MCP (Model Context Protocol) server",
		Long:  "Start an MCP server that exposes the agent via the Model Context Protocol. By default, uses stdio transport. Use --http to start a streaming HTTP server instead. Use --attach to expose a running TUI's session instead of an agent file.",
		Example: `  docker-agent serve mcp ./agent.yaml
  docker-agent serve mcp ./team.yaml
  docker-agent serve mcp agentcatalog/pirate
  docker-agent serve mcp ./agent.yaml --http --listen 127.0.0.1:9090
  docker-agent serve mcp --attach`,
		Args: cobra.MaximumNArgs(1),
		RunE: flags.runMCPCommand,
	}

	cmd.PersistentFlags().StringVarP(&flags.agentName, "agent", "a", "", "Name of the agent to run (all agents if not specified)")
	cmd.PersistentFlags().BoolVar(&flags.http, "http", false, "Use streaming HTTP transport instead of stdio")
	cmd.PersistentFlags().StringVarP(&flags.listenAddr, "listen", "l", "127.0.0.1:8081", "Address to listen on")
	cmd.PersistentFlags().StringVar(&flags.attach, "attach", "", "Attach to a running TUI run by pid, address, or session id (or empty for the most recent)")
	cmd.PersistentFlags().Lookup("attach").NoOptDefVal = "latest"
	cmd.PersistentFlags().StringVar(&flags.runConfig.MCPToolName, "tool-name", "", "Override the MCP tool identifier clients call (defaults to agent name); only valid when exposing a single agent")
	cmd.PersistentFlags().DurationVar(&flags.runConfig.MCPKeepAlive, "mcp-keepalive", 0, "Interval between MCP keep-alive pings (e.g. 30s); 0 disables keep-alive")
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func (f *mcpFlags) runMCPCommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "serve", append([]string{"mcp"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(ctx, "serve", append([]string{"mcp"}, args...), commandErr)
	}()

	if f.attach != "" {
		return f.runAttach(ctx)
	}

	if len(args) == 0 {
		return errors.New("agent file is required (or use --attach)")
	}
	agentFilename := args[0]

	if !f.http {
		return mcp.StartMCPServer(ctx, agentFilename, f.agentName, &f.runConfig)
	}

	ln, cleanup, err := newListener(ctx, f.listenAddr)
	if err != nil {
		return err
	}
	defer cleanup()

	return mcp.StartHTTPServer(ctx, agentFilename, f.agentName, &f.runConfig, ln)
}

func (f *mcpFlags) runAttach(ctx context.Context) error {
	target := f.attach
	if target == "latest" {
		target = ""
	}
	rec, err := runregistry.Find(target)
	if err != nil {
		return err
	}
	return mcp.StartAttachStdio(ctx, rec.Addr, rec.SessionID)
}
