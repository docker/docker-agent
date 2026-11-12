package root

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/sandbox"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func validateSandboxOptions(cmd *cobra.Command, enabled bool, opts sandbox.Options) error {
	for _, name := range []string{"sandbox-kit", "template"} {
		if flag := cmd.Flags().Lookup(name); flag != nil && flag.Changed && strings.TrimSpace(flag.Value.String()) == "" {
			return fmt.Errorf("--%s must not be empty", name)
		}
	}
	for _, ref := range opts.Kits {
		if strings.TrimSpace(ref) == "" {
			return errors.New("--kit must not be empty")
		}
	}
	for _, flag := range []string{"sandbox-kit", "kit", "kit-arg", "sandbox-ttl"} {
		if cmd.Flags().Changed(flag) && !enabled {
			return fmt.Errorf("--%s requires --sandbox", flag)
		}
	}
	if cmd.Flags().Changed("sandbox-ttl") && !opts.Cloud {
		return errors.New("--sandbox-ttl requires --cloud")
	}
	if opts.Cloud && (opts.TTL <= 0 || opts.TTL > 24*time.Hour) {
		return errors.New("--sandbox-ttl must be greater than zero and at most 24h")
	}
	if len(opts.KitArgs) > 0 && opts.Workload == "" && len(opts.Kits) == 0 {
		return errors.New("--kit-arg requires --sandbox-kit or --kit")
	}
	if opts.Cloud && cmd.Flags().Changed("template") && len(opts.Kits) > 0 {
		return errors.New("--cloud --template cannot be combined with --kit; use --sandbox-kit for a composed workload")
	}
	return nil
}

// Cloud runs are remote-native: no host cwd, config, auto-kit, or env-provider
// credentials cross the boundary. sbx owns cloud secrets and kit provisioning.
func cloudAgentArgs(cmd *cobra.Command, args []string) ([]string, error) {
	for _, flag := range []string{
		"working-dir", "config-dir", "cache-dir", "data-dir", "env-from-file",
		"attach", "prompt-file", "session-db", "session", "record", "fake",
		"cpuprofile", "memprofile", "agent-picker", "models-gateway", "listen", "session-workingdir-root",
	} {
		if cmd.Flags().Changed(flag) {
			return nil, fmt.Errorf("--%s is not supported with --cloud; configure remote paths and credentials in a sandbox kit", flag)
		}
	}
	ref := "default"
	if len(args) > 0 {
		ref = args[0]
	}
	userCfg, err := userconfig.Load()
	if err != nil {
		return nil, fmt.Errorf("loading user config: %w", err)
	}
	source, alias, err := sources.ResolveWithConfig(ref, userCfg, nil)
	if err != nil {
		return nil, fmt.Errorf("resolving cloud agent: %w", err)
	}
	if source.ParentDir() != "" {
		return nil, fmt.Errorf("cloud sandboxes cannot read local agent %q; publish it with `docker agent share push` and pass its OCI reference, or use a built-in agent", ref)
	}
	if slices.Contains(sources.BuiltinAgentNames(), source.Name()) {
		models, _ := cmd.Flags().GetStringArray("model")
		if len(models) == 0 && (alias == nil || alias.Model == "") {
			return nil, errors.New("built-in cloud agents require --model (for example --model openai/gpt-5.6); proxy sentinels do not indicate which cloud provider secrets are configured")
		}
	}
	forward := []string{source.Name()}
	if len(args) > 1 {
		forward = append(forward, args[1:]...)
	}

	return dockerAgentArgs(cmd, forward, "", alias), nil
}

func runInCloudSandbox(ctx context.Context, cmd *cobra.Command, args []string, backend *sandbox.Backend, opts sandbox.Options) (retErr error) {
	innerArgs, err := cloudAgentArgs(cmd, args)
	if err != nil {
		return err
	}
	name, createErr := backend.CreateCloud(ctx, opts, cmd.ErrOrStderr())
	if name == "" {
		return sandboxCommandError(ctx, createErr)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Cloud sandbox: %s (no host files uploaded; stopped when this run ends)\n", name)
	defer func() {
		// Cancellation must not leave paid compute running; TTL is the fallback
		// if the client dies before it can stop the sandbox.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := backend.Stop(cleanupCtx, name, cmd.ErrOrStderr()); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %v; stop it with `sbx --cloud stop %s`\n", err, name)
			if retErr == nil {
				retErr = err
			}
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Remote files remain in %s. After stopping completes, restart with `sbx --cloud exec %s true` before copying files with `sbx --cloud cp`; remove with `sbx --cloud rm --force %s`.\n", name, name, name)
	}()
	if createErr != nil {
		return sandboxCommandError(ctx, createErr)
	}
	ctx, span := genai.StartSandboxExec(ctx, genai.SandboxOptions{Runtime: "cloud", Container: name})
	defer span.End()
	envFlags := append([]string{"-e", "SANDBOX_VM_ID=" + name}, genai.InjectSandboxEnv(ctx)...)
	inner := backend.BuildExecCmd(ctx, name, "", sandboxTTY(cmd), innerArgs, envFlags, nil)
	inner.Stdin = cmd.InOrStdin()
	inner.Stdout = cmd.OutOrStdout()
	inner.Stderr = cmd.ErrOrStderr()
	if err := inner.Run(); err != nil {
		span.RecordError(err, "")
		err = sandboxCommandError(ctx, err)
		span.SetExitCode(sandboxExitCode(err))
		return err
	}
	span.SetExitCode(0)
	return nil
}
