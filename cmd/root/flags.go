package root

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/userconfig"
)

const (
	flagModelsGateway      = "models-gateway"
	envModelsGateway       = "DOCKER_AGENT_MODELS_GATEWAY"
	cagentEnvModelsGateway = "CAGENT_MODELS_GATEWAY"
	envDefaultModel        = "DOCKER_AGENT_DEFAULT_MODEL"
	cagentEnvDefaultModel  = "CAGENT_DEFAULT_MODEL"
)

func addRuntimeConfigFlags(cmd *cobra.Command, runConfig *config.RuntimeConfig) {
	addGatewayFlags(cmd, runConfig)
	cmd.PersistentFlags().StringSliceVar(&runConfig.EnvFiles, "env-from-file", nil, "Set environment variables from file")
	cmd.PersistentFlags().BoolVar(&runConfig.GlobalCodeMode, "code-mode-tools", false, "Provide a single tool to call other tools via Javascript")
	cmd.PersistentFlags().StringVar(&runConfig.WorkingDir, "working-dir", "", "Set the working directory for the session (applies to tools and relative paths)")
	cmd.PersistentFlags().StringArrayVar(&runConfig.HookPreToolUse, "hook-pre-tool-use", nil, "Add a pre-tool-use hook command that runs before every tool call (repeatable)")
	cmd.PersistentFlags().StringArrayVar(&runConfig.HookPostToolUse, "hook-post-tool-use", nil, "Add a post-tool-use hook command that runs after every tool call (repeatable)")
	cmd.PersistentFlags().StringArrayVar(&runConfig.HookSessionStart, "hook-session-start", nil, "Add a session-start hook command (repeatable)")
	cmd.PersistentFlags().StringArrayVar(&runConfig.HookSessionEnd, "hook-session-end", nil, "Add a session-end hook command (repeatable)")
	cmd.PersistentFlags().StringArrayVar(&runConfig.HookOnUserInput, "hook-on-user-input", nil, "Add an on-user-input hook command (repeatable)")
	cmd.PersistentFlags().StringArrayVar(&runConfig.HookStop, "hook-stop", nil, "Add a stop hook command, fired when the model finishes responding (repeatable)")
}

func setupWorkingDirectory(workingDir string) error {
	if workingDir == "" {
		return nil
	}

	absWd, err := filepath.Abs(workingDir)
	if err != nil {
		return fmt.Errorf("invalid working directory: %w", err)
	}

	info, err := os.Stat(absWd)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("working directory does not exist or is not a directory: %s", absWd)
	}

	if err := os.Chdir(absWd); err != nil {
		return fmt.Errorf("failed to change working directory: %w", err)
	}

	_ = os.Setenv("PWD", absWd)
	slog.Debug("Working directory set", "path", absWd)

	return nil
}

func canonize(endpoint string) string {
	return strings.TrimSuffix(strings.TrimSpace(endpoint), "/")
}

// loadUserConfig is the function used to load user configuration.
// It can be overridden in tests.
var loadUserConfig = userconfig.Load

func addGatewayFlags(cmd *cobra.Command, runConfig *config.RuntimeConfig) {
	cmd.PersistentFlags().StringVar(&runConfig.ModelsGateway, flagModelsGateway, "", "Set the models gateway address")

	persistentPreRunE := cmd.PersistentPreRunE
	cmd.PersistentPreRunE = func(_ *cobra.Command, args []string) error {
		ctx := cmd.Context()

		userCfg, err := loadUserConfig()
		if err != nil {
			slog.WarnContext(ctx, "Failed to load user config", "error", err)
			userCfg = &userconfig.Config{}
		}

		env := runConfig.EnvProvider()

		// Precedence: CLI flag > environment variable > user config
		if runConfig.ModelsGateway == "" {
			if gateway, _ := env.Get(ctx, envModelsGateway); gateway != "" {
				runConfig.ModelsGateway = gateway
			} else if gateway, _ := env.Get(ctx, cagentEnvModelsGateway); gateway != "" {
				runConfig.ModelsGateway = gateway
			} else if userCfg.ModelsGateway != "" {
				runConfig.ModelsGateway = userCfg.ModelsGateway
			}
		}
		runConfig.ModelsGateway = canonize(runConfig.ModelsGateway)

		// Precedence for default model: environment variable > user config
		if model, _ := env.Get(ctx, envDefaultModel); model != "" {
			runConfig.DefaultModel = parseModelShorthand(model)
		} else if model, _ := env.Get(ctx, cagentEnvDefaultModel); model != "" {
			runConfig.DefaultModel = parseModelShorthand(model)
		} else if userCfg.DefaultModel != nil {
			runConfig.DefaultModel = &userCfg.DefaultModel.ModelConfig
		}

		if err := setupWorkingDirectory(runConfig.WorkingDir); err != nil {
			return err
		}

		if persistentPreRunE != nil {
			return persistentPreRunE(cmd, args)
		}
		// Walk up the ancestor chain to find and call the nearest PersistentPreRunE.
		// A single cmd.Parent() check is not sufficient when this command is nested
		// more than one level deep (e.g. root → serve → api): the immediate parent
		// may have no PersistentPreRunE, but a grandparent (such as root) might.
		for p := cmd.Parent(); p != nil; p = p.Parent() {
			if p.PersistentPreRunE != nil {
				return p.PersistentPreRunE(cmd, args)
			}
		}

		return nil
	}
}

// parseModelShorthand parses "provider/model" into a ModelConfig
func parseModelShorthand(s string) *latest.ModelConfig {
	if idx := strings.Index(s, "/"); idx > 0 && idx < len(s)-1 {
		return &latest.ModelConfig{
			Provider: s[:idx],
			Model:    s[idx+1:],
		}
	}
	return nil
}

// newListener creates a TCP listener and returns a cleanup function that
// must be deferred by the caller. The cleanup function closes the listener.
// The listener is also closed if the context is cancelled, which unblocks
// any in-progress Serve call.
func newListener(ctx context.Context, addr string) (net.Listener, func(), error) {
	ln, err := server.Listen(ctx, addr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	stop := context.AfterFunc(ctx, func() {
		_ = ln.Close()
	})
	cleanup := func() {
		stop()
		_ = ln.Close()
	}
	return ln, cleanup, nil
}
