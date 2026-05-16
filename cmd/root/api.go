package root

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry"
)

type apiFlags struct {
	listenAddr       string
	sessionDB        string
	pullIntervalMins int
	fakeResponses    string
	recordPath       string
	authToken        string
	runConfig        config.RuntimeConfig
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
	cmd.PersistentFlags().StringVar(&flags.authToken, "auth-token", "", "Bearer token required for API requests (empty = no authentication)")
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

	// Make sure no question is ever asked to the user in api mode.
	os.Stdin = nil

	// Start fake proxy if --fake is specified
	cleanup, err := setupFakeProxy(f.fakeResponses, 0, &f.runConfig)
	if err != nil {
		return err
	}
	defer func() {
		if err := cleanup(); err != nil {
			slog.ErrorContext(ctx, "Failed to cleanup fake proxy", "error", err)
		}
	}()

	// Start recording proxy if --record is specified
	_, recordCleanup, err := setupRecordingProxy(f.recordPath, &f.runConfig)
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

	ln, lnCleanup, err := newListener(ctx, f.listenAddr)
	if err != nil {
		return err
	}
	defer lnCleanup()

	out.Println("Listening on", ln.Addr().String())
	warnIfNotLoopback(out, ln.Addr())

	slog.DebugContext(ctx, "Starting server", "agents", agentsPath, "addr", ln.Addr().String())

	// Expand tilde in session database path
	sessionDB, err := pathx.ExpandHomeDir(f.sessionDB)
	if err != nil {
		return err
	}

	sessionStore, err := session.NewSQLiteSessionStore(sessionDB)
	if err != nil {
		return fmt.Errorf("creating session store: %w", err)
	}
	defer func() {
		if err := sessionStore.Close(); err != nil {
			slog.ErrorContext(ctx, "Failed to close session store", "error", err)
		}
	}()

	sources, err := config.ResolveSources(agentsPath, f.runConfig.EnvProvider())
	if err != nil {
		return fmt.Errorf("resolving agent sources: %w", err)
	}

	s, err := server.New(ctx, sessionStore, &f.runConfig, time.Duration(f.pullIntervalMins)*time.Minute, sources, f.authToken)
	if err != nil {
		return fmt.Errorf("creating server: %w", err)
	}

	return s.Serve(ctx, ln)
}

// warnIfNotLoopback prints a security warning when the API server is bound to
// an address other than loopback. The default --listen value is 127.0.0.1, so
// reaching this code path means the operator was explicit about exposing the
// API; we just remind them that the API has no authentication.
func warnIfNotLoopback(out *cli.Printer, addr net.Addr) {
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		// Unix sockets and named pipes rely on filesystem permissions.
		return
	}
	if tcpAddr.IP.IsLoopback() {
		return
	}
	out.Println("WARNING: API server is listening on a non-loopback address.")
	out.Println("         The API has no authentication; anyone able to reach")
	out.Println("         this address can run agents and access all sessions.")
	slog.Warn("API server bound to non-loopback address", "addr", tcpAddr.String())
}
