package root

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/docker/cli/cli"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/sandbox"
)

// runInSandbox delegates the current command to a Docker sandbox.
// It ensures a sandbox exists (creating or recreating as needed), then
// executes docker agent inside it via the sandbox exec command.
func runInSandbox(ctx context.Context, cmd *cobra.Command, args []string, runConfig *config.RuntimeConfig, template string, preferSbx bool) error {
	if environment.InSandbox() {
		return fmt.Errorf("already running inside a Docker sandbox (VM %s)", os.Getenv("SANDBOX_VM_ID"))
	}

	backend := sandbox.NewBackend(preferSbx)

	if err := backend.CheckAvailable(ctx); err != nil {
		return err
	}

	var agentRef string
	if len(args) > 0 {
		agentRef = args[0]
	}

	configDir := paths.GetConfigDir()
	dockerAgentArgs := dockerAgentArgs(cmd, args, configDir)

	stopTokenWriter := sandbox.StartTokenWriterIfNeeded(ctx, configDir, runConfig.ModelsGateway)
	defer stopTokenWriter()

	// Resolve wd to an absolute path so that it matches the absolute
	// workspace paths returned by `docker sandbox ls --json`.
	wd, err := filepath.Abs(cmp.Or(runConfig.WorkingDir, "."))
	if err != nil {
		return fmt.Errorf("resolving workspace path: %w", err)
	}

	name, err := backend.Ensure(ctx, wd, sandbox.ExtraWorkspace(wd, agentRef), template, configDir)
	if err != nil {
		return err
	}

	// Resolve env vars the agent needs and forward them into the sandbox.
	// Docker Desktop proxies well-known API keys automatically; this handles
	// any additional vars (e.g. MCP tool secrets).
	envFlags, envVars := sandbox.EnvForAgent(ctx, agentRef, environment.NewDefaultProvider())

	// Forward the gateway as an env var so docker sandbox exec sets it
	// directly inside the sandbox.
	if gateway := runConfig.ModelsGateway; gateway != "" {
		envFlags = append(envFlags, "-e", envModelsGateway+"="+gateway)
	}

	dockerCmd := backend.BuildExecCmd(ctx, name, wd, dockerAgentArgs, envFlags, envVars)
	slog.DebugContext(ctx, "Executing in sandbox", "name", name, "args", dockerCmd.Args)

	if err := dockerCmd.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return cli.StatusError{StatusCode: exitErr.ExitCode()}
		}
		return fmt.Errorf("docker sandbox exec failed: %w", err)
	}

	return nil
}

func dockerAgentArgs(cmd *cobra.Command, args []string, configDir string) []string {
	var dockerAgentArgs []string
	hasYolo := false
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if f.Name == "sandbox" || f.Name == "sbx" || f.Name == "config-dir" {
			return
		}

		if f.Name == "yolo" {
			hasYolo = true
		}

		if f.Value.Type() == "bool" {
			dockerAgentArgs = append(dockerAgentArgs, "--"+f.Name)
		} else {
			dockerAgentArgs = append(dockerAgentArgs, "--"+f.Name, f.Value.String())
		}
	})
	if !hasYolo {
		dockerAgentArgs = append(dockerAgentArgs, "--yolo")
	}

	dockerAgentArgs = append(dockerAgentArgs, args...)
	dockerAgentArgs = append(dockerAgentArgs, "--config-dir", configDir)

	return dockerAgentArgs
}
