package root

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/cli/cli"
	"github.com/docker/cli/cli-plugins/metadata"
	"github.com/docker/cli/cli-plugins/plugin"
	"github.com/docker/cli/cli/command"
	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/feedback"
	"github.com/docker/docker-agent/pkg/logging"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/version"
)

type rootFlags struct {
	enableOtel  bool
	debugMode   bool
	logFilePath string
	logFile     io.Closer
	cacheDir    string
	configDir   string
	dataDir     string
}

func NewRootCmd() *cobra.Command {
	var flags rootFlags

	cmd := &cobra.Command{
		Use:   "docker-agent",
		Short: "Docker AI Agent Runner",
		Example: `  docker-agent run
  docker-agent run ./agent.yaml
  docker-agent run agentcatalog/pirate`,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Apply directory overrides before anything else so that
			// logging, telemetry, and config loading honour them.
			if dir := flags.cacheDir; dir != "" {
				paths.SetCacheDir(dir)
			}
			if dir := flags.configDir; dir != "" {
				paths.SetConfigDir(dir)
			}
			if dir := flags.dataDir; dir != "" {
				paths.SetDataDir(dir)
			}

			// Set the version for automatic telemetry initialization
			telemetry.SetGlobalTelemetryVersion(version.Version)

			// Print startup message only on first installation/setup
			if isFirstRun() && os.Getenv("CAGENT_HIDE_TELEMETRY_BANNER") != "1" && os.Getenv("DOCKER_AGENT_HIDE_TELEMETRY_BANNER") != "1" {
				welcomeMsg := fmt.Sprintf(`
Welcome to docker agent! 🚀

For any feedback, please visit: %s
`, feedback.Link)
				fmt.Fprint(cmd.ErrOrStderr(), welcomeMsg)

				// Only show telemetry notice when telemetry is enabled
				if telemetry.GetTelemetryEnabled() {
					telemetryMsg := `
We collect anonymous usage data to help improve docker agent. To disable:
  - Set environment variable: TELEMETRY_ENABLED=false
`
					fmt.Fprint(cmd.ErrOrStderr(), telemetryMsg)
				}

				fmt.Fprintln(cmd.ErrOrStderr())
			}

			// Initialize logging before anything else so logs don't break TUI
			if err := flags.setupLogging(); err != nil {
				// If logging setup fails, fall back to stderr so we still get logs
				slog.SetDefault(slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{
					Level: func() slog.Level {
						if flags.debugMode {
							return slog.LevelDebug
						}
						return slog.LevelInfo
					}(),
				})))
			}

			telemetry.SetGlobalTelemetryDebugMode(flags.debugMode)

			if flags.enableOtel {
				if err := initOTelSDK(cmd.Context()); err != nil {
					slog.Warn("Failed to initialize OpenTelemetry SDK", "error", err)
				} else {
					slog.Debug("OpenTelemetry SDK initialized successfully")
				}
			}

			return nil
		},
		PersistentPostRunE: func(cmd *cobra.Command, args []string) error {
			if flags.logFile != nil {
				if err := flags.logFile.Close(); err != nil {
					slog.Error("Failed to close log file", "error", err)
				}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Default to "run" command
			if len(args) == 0 {
				runCmd, _, _ := cmd.Find([]string{"run"})
				if err := runCmd.PersistentPreRunE(runCmd, nil); err != nil {
					return err
				}
				return runCmd.RunE(runCmd, nil)
			}

			// Or print help
			if args[0] == "help" {
				return cmd.Help()
			}

			// Or print help and an unknown command error
			_ = cmd.Help()
			return cli.StatusError{
				StatusCode: 1,
				Status:     fmt.Sprintf("ERROR: unknown command: %q", args[0]),
			}
		},
		SilenceUsage: true,
	}

	// Add persistent debug flag available to all commands
	cmd.PersistentFlags().BoolVarP(&flags.debugMode, "debug", "d", false, "Enable debug logging")
	cmd.PersistentFlags().BoolVarP(&flags.enableOtel, "otel", "o", false, "Enable OpenTelemetry tracing")
	cmd.PersistentFlags().StringVar(&flags.logFilePath, "log-file", "", "Path to debug log file (default: ~/.cagent/cagent.debug.log; only used with --debug)")
	cmd.PersistentFlags().StringVar(&flags.cacheDir, "cache-dir", "", "Override the cache directory (default: ~/Library/Caches/cagent on macOS)")
	cmd.PersistentFlags().StringVar(&flags.configDir, "config-dir", "", "Override the config directory (default: ~/.config/cagent)")
	cmd.PersistentFlags().StringVar(&flags.dataDir, "data-dir", "", "Override the data directory (default: ~/.cagent)")

	// Define groups
	cmd.AddGroup(
		&cobra.Group{ID: "core", Title: "Core Commands:"},
		&cobra.Group{ID: "advanced", Title: "Advanced Commands:"},
	)

	cmd.AddCommand(
		newVersionCmd(),
		newRunCmd(),
		newNewCmd(),
		newEvalCmd(),
		newShareCmd(),
		newModelsCmd(),
		newDebugCmd(),
		newAliasCmd(),
		newServeCmd(),
	)

	return cmd
}

func Execute(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	rootCmd := NewRootCmd()
	rootCmd.SetIn(stdin)
	rootCmd.SetOut(stdout)
	rootCmd.SetErr(stderr)
	rootCmd.SetArgs(args)

	runningStandalone := plugin.RunningStandalone()

	visitAll(rootCmd, func(cmd *cobra.Command) {
		cmd.SetContext(ctx)
		if !runningStandalone {
			cmd.Example = strings.ReplaceAll(cmd.Example, "docker-agent", "docker agent")
		}
	})

	if runningStandalone {
		return rootCmd.Execute()
	}

	plugin.Run(func(command.Cli) *cobra.Command {
		// Force to the name of the docker command
		rootCmd.Use = "agent"

		// Force default usage template. Otherwise it gets overridden by docker's.
		rootCmd.SetUsageTemplate(rootCmd.UsageTemplate())

		originalPreRun := rootCmd.PersistentPreRunE
		rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
			if err := plugin.PersistentPreRunE(cmd, args); err != nil {
				return err
			}
			if originalPreRun != nil {
				return originalPreRun(cmd, args)
			}
			return nil
		}
		return rootCmd
	}, metadata.Metadata{
		SchemaVersion: "0.1.0",
		Vendor:        "Docker Inc.",
		Version:       version.Version,
	})

	return nil
}

func visitAll(cmd *cobra.Command, fn func(*cobra.Command)) {
	fn(cmd)
	for _, cmd := range cmd.Commands() {
		visitAll(cmd, fn)
	}
}

// setupLogging configures slog logging behavior.
// When --debug is enabled, logs are written to a rotating file <dataDir>/cagent.debug.log,
// or to the file specified by --log-file. Log files are rotated when they exceed 10MB,
// keeping up to 3 backup files.
func (f *rootFlags) setupLogging() error {
	if !f.debugMode {
		slog.SetDefault(slog.New(slog.DiscardHandler))
		return nil
	}

	path := cmp.Or(strings.TrimSpace(f.logFilePath), filepath.Join(paths.GetDataDir(), "cagent.debug.log"))

	logFile, err := logging.NewRotatingFile(path)
	if err != nil {
		return err
	}
	f.logFile = logFile

	slog.SetDefault(slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug})))

	return nil
}

// RuntimeError wraps runtime errors to distinguish them from usage errors
type RuntimeError struct {
	Err error
}

func (e RuntimeError) Error() string {
	return e.Err.Error()
}

func (e RuntimeError) Unwrap() error {
	return e.Err
}

// isFirstRun checks if this is the first time docker agent is being run.
// It atomically creates a marker file in the user's config directory
// using os.O_EXCL to avoid a race condition when multiple processes
// start concurrently.
func isFirstRun() bool {
	configDir := paths.GetConfigDir()
	markerFile := filepath.Join(configDir, ".cagent_first_run")

	// Ensure the config directory exists before trying to create the marker file
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		slog.Warn("Failed to create config directory for first run marker", "error", err)
		return false
	}

	// Atomically create the marker file. If it already exists, OpenFile returns an error.
	f, err := os.OpenFile(markerFile, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // empty marker file with no sensitive content
	if err != nil {
		return false // File already exists or other error, not first run
	}
	if err := f.Close(); err != nil {
		slog.Warn("Failed to close first run marker file", "error", err)
	}

	return true
}
