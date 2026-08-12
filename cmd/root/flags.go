package root

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/paths"
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
	addGatewayFlags(cmd, runConfig, userconfig.Load)
	cmd.PersistentFlags().StringSliceVar(&runConfig.EnvFiles, "env-from-file", nil, "Set environment variables from file")
	cmd.PersistentFlags().StringArrayVar(&runConfig.Flavors, "flavor", nil, "Enable a config flavor, a YAML patch defined under the config's 'flavors' section (repeatable, applied in order)")
	cmd.PersistentFlags().BoolVar(&runConfig.GlobalCodeMode, "code-mode-tools", false, "Provide a single tool to call other tools via Javascript")
	cmd.PersistentFlags().StringVar(&runConfig.WorkingDir, "working-dir", "", "Set the working directory for the session (applies to tools and relative paths)")
	cmd.PersistentFlags().StringArrayVar(&runConfig.HookPreToolUse, "hook-pre-tool-use", nil, "Add a pre-tool-use hook command that runs before every tool call (repeatable)")
	cmd.PersistentFlags().StringArrayVar(&runConfig.HookPostToolUse, "hook-post-tool-use", nil, "Add a post-tool-use hook command that runs after every tool call (repeatable)")
	cmd.PersistentFlags().StringArrayVar(&runConfig.HookSessionStart, "hook-session-start", nil, "Add a session-start hook command (repeatable)")
	cmd.PersistentFlags().StringArrayVar(&runConfig.HookSessionEnd, "hook-session-end", nil, "Add a session-end hook command (repeatable)")
	cmd.PersistentFlags().StringArrayVar(&runConfig.HookOnUserInput, "hook-on-user-input", nil, "Add an on-user-input hook command (repeatable)")
	cmd.PersistentFlags().StringArrayVar(&runConfig.HookStop, "hook-stop", nil, "Add a stop hook command, fired when the model finishes responding (repeatable)")
	cmd.PersistentFlags().StringVar(&runConfig.MCPOAuthRedirectURI, "mcp-oauth-redirect-uri", "",
		"Public HTTPS URL to advertise as the OAuth `redirect_uri` for MCP servers "+
			"running in unmanaged OAuth mode. When set, docker-agent drives the OAuth flow "+
			"itself (PKCE + DCR + token exchange) and expects clients to return `{code, state}` "+
			"via ResumeElicitation. When empty, the client is expected to perform the OAuth "+
			"flow and return an access token (legacy behavior).")
}

// sessionDBPath resolves the session database path from the --session-db
// flag. The default is resolved lazily, at command run time, because the
// data dir must reflect a --data-dir override applied by the root
// PersistentPreRunE; a default baked in at flag registration would always
// point at ~/.cagent regardless of --data-dir.
func sessionDBPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return filepath.Join(paths.GetDataDir(), "session.db")
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

// validateSessionWorkingDirRoot rejects a --session-workingdir-root value
// that trims to empty (e.g. an unresolved shell variable): the operator
// asked for containment, so silently disabling it would be invisible and
// dangerous. An empty value (flag unset) means unrestricted and is valid.
func validateSessionWorkingDirRoot(root string) error {
	if root != "" && strings.TrimSpace(root) == "" {
		return errors.New("--session-workingdir-root: value is empty after trimming whitespace")
	}
	return nil
}

func canonize(endpoint string) string {
	return strings.TrimSuffix(strings.TrimSpace(endpoint), "/")
}

// userConfigLoader loads the user configuration. Production passes
// [userconfig.Load]; tests inject a stub so they neither touch the real
// config file nor share a mutable package global (and can run in parallel).
type userConfigLoader func() (*userconfig.Config, error)

func addGatewayFlags(cmd *cobra.Command, runConfig *config.RuntimeConfig, loadUserConfig userConfigLoader) {
	cmd.PersistentFlags().StringVar(&runConfig.ModelsGateway, flagModelsGateway, "", "Set the models gateway address")

	persistentPreRunE := cmd.PersistentPreRunE
	cmd.PersistentPreRunE = func(_ *cobra.Command, args []string) error {
		ctx := cmd.Context()

		// Run any inherited PersistentPreRunE first so directory
		// overrides (--config-dir, --cache-dir, --data-dir) and other
		// global setup land before we materialise the env provider —
		// otherwise the cached provider chain captures a stale config
		// dir and breaks downstream consumers like the sandbox token
		// reader.
		if err := runParentPreRun(cmd, persistentPreRunE, args); err != nil {
			return err
		}

		userCfg, err := loadUserConfig()
		if err != nil {
			slog.WarnContext(ctx, "Failed to load user config", "error", err)
			userCfg = &userconfig.Config{}
		}

		env := runConfig.EnvProvider()

		// A missing or malformed --env-from-file must abort the run: the flag
		// is the documented way to supply credentials, so silently continuing
		// without it leads to confusing "env var must be set" errors later.
		if err := runConfig.EnvFilesError(); err != nil {
			return fmt.Errorf("--env-from-file: %w", err)
		}

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

		// User-level custom providers (registered via `docker agent setup` or
		// hand-written in the user config) apply to every run; agent-file
		// definitions with the same name win when configs are loaded.
		if runConfig.Providers == nil {
			runConfig.Providers = userCfg.GetProviders()
		}

		return setupWorkingDirectory(runConfig.WorkingDir)
	}
}

// runParentPreRun runs the cobra PersistentPreRunE that should fire
// before this command's own. It first invokes the directly-captured
// hook (if any) and otherwise walks up the ancestor chain to find the
// nearest PersistentPreRunE — which matters when the command is nested
// more than one level deep (e.g. root → serve → api): the immediate
// parent may have no PersistentPreRunE, but a grandparent (such as
// root) might.
func runParentPreRun(cmd *cobra.Command, captured func(*cobra.Command, []string) error, args []string) error {
	if captured != nil {
		return captured(cmd, args)
	}
	for p := cmd.Parent(); p != nil; p = p.Parent() {
		if p.PersistentPreRunE != nil {
			return p.PersistentPreRunE(cmd, args)
		}
	}
	return nil
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
