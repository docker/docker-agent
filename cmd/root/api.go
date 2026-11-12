package root

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/httpsec"
	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/profiling"
	"github.com/docker/docker-agent/pkg/runtime/eventmerge"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/telemetry"
)

const (
	envPprofAddr       = "DOCKER_AGENT_PPROF_ADDR"
	cagentEnvPprofAddr = "CAGENT_PPROF_ADDR"
)

type apiFlags struct {
	listenAddr            string
	sessionDB             string
	pullIntervalMins      int
	fakeResponses         string
	recordPath            string
	authToken             string
	pprofAddr             string
	maxRequestSize        int64
	sessionWorkingDirRoot string
	streamCoalesceWindow  time.Duration
	runConfig             config.RuntimeConfig
}

func newAPICmd() *cobra.Command {
	var flags apiFlags

	cmd := &cobra.Command{
		Use:   "api <agent-file>|<agents-dir>",
		Short: "Start the API server",
		Args:  cobra.ExactArgs(1),
		RunE:  flags.runAPICommand,
	}

	cmd.PersistentFlags().StringVarP(&flags.listenAddr, "listen", "l", "127.0.0.1:8080", "Address to listen on")
	cmd.PersistentFlags().StringVarP(&flags.sessionDB, "session-db", "s", "session.db", "Path to the session database")
	cmd.PersistentFlags().IntVar(&flags.pullIntervalMins, "pull-interval", 0, "Auto-pull OCI reference every N minutes (0 = disabled)")
	cmd.PersistentFlags().StringVar(&flags.fakeResponses, "fake", "", "Replay AI responses from cassette file (for testing)")
	cmd.PersistentFlags().StringVar(&flags.recordPath, "record", "", "Record AI API interactions to cassette file")
	cmd.PersistentFlags().StringVar(&flags.authToken, "auth-token", "", "Bearer token required for API requests (required for non-loopback TCP listeners)")
	cmd.PersistentFlags().Int64Var(&flags.maxRequestSize, "max-request-size", 1<<20, "Maximum request body size in bytes (default 1 MiB). Requests exceeding this limit are rejected with HTTP 413.")
	cmd.PersistentFlags().StringVar(&flags.sessionWorkingDirRoot, "session-workingdir-root", "", "Restrict the working_dir of sessions created via POST /api/sessions to this directory and its descendants (empty = no restriction; recommended for multi-user deployments)")
	cmd.PersistentFlags().DurationVar(&flags.streamCoalesceWindow, "stream-coalesce-window", eventmerge.DefaultWindow, "Merge consecutive streaming deltas (text, reasoning, tool-call arguments, tool output) arriving within this window into one SSE event (0 = one event per delta)")
	cmd.PersistentFlags().StringVar(&flags.pprofAddr, "pprof-addr", "", "TCP host:port to expose Go pprof endpoints at /debug/pprof/ (e.g. 127.0.0.1:6060); also set via DOCKER_AGENT_PPROF_ADDR")
	_ = cmd.PersistentFlags().MarkHidden("pprof-addr")
	cmd.MarkFlagsMutuallyExclusive("fake", "record")
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func (f *apiFlags) runAPICommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "serve", append([]string{"api"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(ctx, "serve", append([]string{"api"}, args...), commandErr)
	}()

	out := cli.NewPrinter(cmd.OutOrStdout())
	agentsPath := args[0]

	if err := validateSessionWorkingDirRoot(f.sessionWorkingDirRoot); err != nil {
		return err
	}

	// Make sure no question is ever asked to the user in api mode.
	os.Stdin = nil

	// Start fake proxy if --fake is specified
	cleanup, err := setupFakeProxy(ctx, f.fakeResponses, 0, &f.runConfig)
	if err != nil {
		return err
	}
	defer func() {
		if err := cleanup(); err != nil {
			slog.ErrorContext(ctx, "Failed to cleanup fake proxy", "error", err)
		}
	}()

	// Start recording proxy if --record is specified
	_, recordCleanup, err := setupRecordingProxy(ctx, f.recordPath, &f.runConfig)
	if err != nil {
		return err
	}
	defer func() {
		if err := recordCleanup(); err != nil {
			slog.ErrorContext(ctx, "Failed to cleanup recording proxy", "error", err)
		}
	}()

	if f.pullIntervalMins > 0 && !config.IsOCIReference(agentsPath) && !config.IsURLReference(agentsPath) {
		return errors.New("--pull-interval flag can only be used with OCI or URL references, not local files")
	}

	if pprofAddr := cmp.Or(f.pprofAddr, os.Getenv(envPprofAddr), os.Getenv(cagentEnvPprofAddr)); pprofAddr != "" {
		if err := profiling.StartPprofServer(ctx, pprofAddr); err != nil {
			return err
		}
	}

	ln, lnCleanup, err := newListener(ctx, f.listenAddr)
	if err != nil {
		return err
	}
	defer lnCleanup()

	if err := httpsec.ValidateListenerAuth(ln.Addr(), f.authToken); err != nil {
		return err
	}
	out.Println("Listening on", ln.Addr().String())

	slog.DebugContext(ctx, "Starting server", "agents", agentsPath, "addr", ln.Addr().String())

	// Expand tilde in session database path
	sessionDB, err := pathx.ExpandHomeDir(f.sessionDB)
	if err != nil {
		return err
	}

	sessionStore, err := sqlitestore.New(ctx, sessionDB)
	if err != nil {
		return fmt.Errorf("creating session store: %w", err)
	}
	defer func() {
		if err := sessionStore.Close(); err != nil {
			slog.ErrorContext(ctx, "Failed to close session store", "error", err)
		}
	}()

	agentSources, err := sources.ResolveSources(agentsPath, f.runConfig.EnvProvider())
	if err != nil {
		return fmt.Errorf("resolving agent sources: %w", err)
	}

	s, err := server.New(ctx, sessionStore, &f.runConfig, time.Duration(f.pullIntervalMins)*time.Minute, agentSources, f.authToken, f.maxRequestSize,
		server.WithSessionWorkingDirRoot(f.sessionWorkingDirRoot),
		server.WithStreamCoalesceWindow(f.streamCoalesceWindow))
	if err != nil {
		return fmt.Errorf("creating server: %w", err)
	}

	return s.Serve(ctx, ln)
}
