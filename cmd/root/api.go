package root

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
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
	cmd.MarkFlagsMutuallyExclusive("fake", "record")
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func (f *apiFlags) runAPICommand(cmd *cobra.Command, args []string) error {
	telemetry.TrackCommand("serve", append([]string{"api"}, args...))

	ctx := cmd.Context()

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
			slog.Error("Failed to cleanup fake proxy", "error", err)
		}
	}()

	// Start recording proxy if --record is specified
	_, recordCleanup, err := setupRecordingProxy(f.recordPath, &f.runConfig)
	if err != nil {
		return err
	}
	defer func() {
		if err := recordCleanup(); err != nil {
			slog.Error("Failed to cleanup recording proxy", "error", err)
		}
	}()

	if f.pullIntervalMins > 0 && !config.IsOCIReference(agentsPath) {
		return errors.New("--pull-interval flag can only be used with OCI references, not local files")
	}

	ln, lnCleanup, err := newListener(ctx, f.listenAddr)
	if err != nil {
		return err
	}
	defer lnCleanup()

	out.Println("Listening on", ln.Addr().String())

	slog.Debug("Starting server", "agents", agentsPath, "addr", ln.Addr().String())

	// Expand tilde in session database path
	sessionDB, err := expandTilde(f.sessionDB)
	if err != nil {
		return err
	}

	sessionStore, err := session.NewSQLiteSessionStore(sessionDB)
	if err != nil {
		return fmt.Errorf("creating session store: %w", err)
	}

	sources, err := config.ResolveSources(agentsPath, f.runConfig.EnvProvider())
	if err != nil {
		if closeErr := sessionStore.Close(); closeErr != nil {
			slog.Error("Failed to close session store", "error", closeErr)
		}
		return fmt.Errorf("resolving agent sources: %w", err)
	}

	s, err := server.New(ctx, sessionStore, &f.runConfig, time.Duration(f.pullIntervalMins)*time.Minute, sources)
	if err != nil {
		if closeErr := sessionStore.Close(); closeErr != nil {
			slog.Error("Failed to close session store", "error", closeErr)
		}
		return fmt.Errorf("creating server: %w", err)
	}

	if err := s.Serve(ctx, ln); err != nil {
		if closeErr := sessionStore.Close(); closeErr != nil {
			slog.Error("Failed to close session store", "error", closeErr)
		}
		return err
	}

	// Close session store on successful completion
	if err := sessionStore.Close(); err != nil {
		slog.Error("Failed to close session store", "error", err)
	}
	return nil
}
