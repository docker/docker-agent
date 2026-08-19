package root

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/a2a"
	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/httpsec"
	"github.com/docker/docker-agent/pkg/servesafety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry"
)

type a2aFlags struct {
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

func newA2ACmd() *cobra.Command {
	var flags a2aFlags

	cmd := &cobra.Command{
		Use:   "a2a <agent-file>|<registry-ref>",
		Short: "Start an agent as an A2A (Agent-to-Agent) server",
		Long:  "Start an A2A server that exposes the agent via the Agent-to-Agent protocol",
		Example: `  docker-agent serve a2a ./agent.yaml
  docker-agent serve a2a myorg/agent:tag --listen 127.0.0.1:9090`,
		Args: cobra.ExactArgs(1),
		RunE: flags.runA2ACommand,
	}

	cmd.PersistentFlags().StringVarP(&flags.agentName, "agent", "a", "", "Name of the agent to run (defaults to the team's first agent)")
	cmd.PersistentFlags().StringVarP(&flags.listenAddr, "listen", "l", "127.0.0.1:8082", "Address to listen on")
	cmd.PersistentFlags().StringVarP(&flags.sessionDB, "session-db", "s", "", "Path to the session database (default: <data-dir>/session.db)")
	cmd.PersistentFlags().StringVar(&flags.safety, "safety", "", "Tool safety policy (strict, balanced, restricted, autonomous)")
	cmd.PersistentFlags().StringVar(&flags.authToken, "auth-token", "", "Bearer token required for all A2A requests")
	cmd.PersistentFlags().StringVar(&flags.corsOrigin, "cors-origin", "", "Allowed browser origin(s), comma-separated; empty disables CORS")
	cmd.PersistentFlags().BoolVar(&flags.insecureNoAuth, "insecure-no-auth", false, "Allow unauthenticated non-loopback binding (insecure)")
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func (f *a2aFlags) runA2ACommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "serve", append([]string{"a2a"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(ctx, "serve", append([]string{"a2a"}, args...), commandErr)
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
		return errors.New("non-loopback A2A listeners require --auth-token or --insecure-no-auth")
	}

	out := cli.NewPrinter(f.stdout)
	if f.stdout == nil {
		out = cli.NewPrinter(cmd.OutOrStdout())
	}
	agentFilename := args[0]

	ln, cleanup, err := newListener(ctx, f.listenAddr)
	if err != nil {
		return err
	}
	defer cleanup()

	out.Println("Listening on", ln.Addr().String())
	return a2a.Run(ctx, agentFilename, f.agentName, sessionDBPath(f.sessionDB), &f.runConfig, ln, a2a.RunOptions{
		CLISafety:  session.SafetyPolicy(f.safety),
		AuthToken:  f.authToken,
		CORSOrigin: f.corsOrigin,
		OnSafetyPolicy: func(resolved servesafety.Resolved) {
			out.Printf("Tool safety policy: %s (source: %s)\n", resolved.Policy, resolved.Source)
		},
	})
}

func isLoopbackListenAddr(addr string) bool {
	if strings.HasPrefix(addr, "unix://") {
		return true
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
