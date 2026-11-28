package root

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/agui"
	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/httpsec"
	"github.com/docker/docker-agent/pkg/servesafety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry"
)

type aguiFlags struct {
	agentName      string
	listenAddr     string
	sessionDB      string
	safety         string
	authToken      string
	corsOrigin     string
	insecureNoAuth bool
	stdout         io.Writer
	runConfig      config.RuntimeConfig
}

func newAGUICmd() *cobra.Command {
	var flags aguiFlags

	cmd := &cobra.Command{
		Use:   "ag-ui <agent-file>|<registry-ref>",
		Short: "Start an agent as an AG-UI server",
		Long: `Start an HTTP server that exposes the agent through the AG-UI protocol:
POST a RunAgentInput to /ag-ui and the run is streamed back as AG-UI events
over Server-Sent Events, for any AG-UI client SDK to consume.`,
		Example: `  docker-agent serve ag-ui ./agent.yaml
  docker-agent serve ag-ui ./team.yaml --agent reviewer
  docker-agent serve ag-ui myorg/agent:tag --listen 127.0.0.1:9090`,
		Args: cobra.ExactArgs(1),
		RunE: flags.runAGUICommand,
	}

	cmd.Flags().StringVarP(&flags.agentName, "agent", "a", "", "Name of the agent to run (defaults to the team's first agent)")
	cmd.Flags().StringVarP(&flags.listenAddr, "listen", "l", "127.0.0.1:8084", "Address to listen on")
	cmd.Flags().StringVarP(&flags.sessionDB, "session-db", "s", "", "Path to the session database (default: <data-dir>/session.db)")
	cmd.Flags().StringVar(&flags.safety, "safety", "", "Tool safety policy (strict, balanced, restricted, autonomous)")
	cmd.Flags().StringVar(&flags.authToken, "auth-token", "", "Bearer token required for all AG-UI requests")
	cmd.Flags().StringVar(&flags.corsOrigin, "cors-origin", "", "Allowed browser origin(s), comma-separated; empty disables CORS")
	cmd.Flags().BoolVar(&flags.insecureNoAuth, "insecure-no-auth", false, "Allow unauthenticated non-loopback binding (insecure)")
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func (f *aguiFlags) runAGUICommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "serve", append([]string{"ag-ui"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(ctx, "serve", append([]string{"ag-ui"}, args...), commandErr)
	}()

	if err := validateSafetyFlag(f.safety); err != nil {
		return err
	}
	if f.corsOrigin != "" {
		if _, err := httpsec.ParseOrigins(f.corsOrigin); err != nil {
			return fmt.Errorf("invalid --cors-origin: %w", err)
		}
	}
	if !isLoopbackListenAddr(f.listenAddr) && f.authToken == "" && !f.insecureNoAuth {
		return errors.New("non-loopback AG-UI listeners require --auth-token or --insecure-no-auth")
	}

	out := cli.NewPrinter(f.stdout)
	if f.stdout == nil {
		out = cli.NewPrinter(cmd.OutOrStdout())
	}

	ln, cleanup, err := newListener(ctx, f.listenAddr)
	if err != nil {
		return err
	}
	defer cleanup()

	out.Println("Listening on", ln.Addr().String())
	out.Println("AG-UI endpoint: http://" + ln.Addr().String() + agui.Path)
	return agui.Run(ctx, args[0], f.agentName, sessionDBPath(f.sessionDB), &f.runConfig, ln, agui.RunOptions{
		CLISafety:  session.SafetyPolicy(f.safety),
		AuthToken:  f.authToken,
		CORSOrigin: f.corsOrigin,
		OnSafetyPolicy: func(resolved servesafety.Resolved) {
			out.Printf("Tool safety policy: %s (source: %s)\n", resolved.Policy, resolved.Source)
		},
	})
}
