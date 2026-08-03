package root

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/cli/invocation"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// errSandboxHostAbsent aborts a deny update before the save so removing an
// already-absent host never rewrites the config file.
var errSandboxHostAbsent = errors.New("host not on the allowlist")

// newSandboxCmd assembles the `docker agent sandbox` subcommand
// group. The group is intentionally narrow today: it only manages
// the persistent network allowlist users build up via
// `docker agent sandbox allow <host>` so a 403 from the in-sandbox
// proxy can be turned into a one-line fix that survives across runs.
// VM lifecycle commands (ls / rm / prune) live with the backend CLI.
func newSandboxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sandbox",
		Short:   "Manage docker-agent sandbox settings",
		Long:    "Manage persistent sandbox network allowlist entries shared across runs.",
		GroupID: "advanced",
	}
	cmd.AddCommand(newSandboxAllowCmd())
	cmd.AddCommand(newSandboxDenyCmd())
	cmd.AddCommand(newSandboxListCmd())
	return cmd
}

func newSandboxAllowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "allow <host> [<host>...]",
		Short: "Add host(s) to the persistent sandbox network allowlist",
		Long: `Add hosts to the user-level sandbox network allowlist.

Listed hosts are added to the sandbox proxy's allow rules on every
subsequent --sandbox run, in addition to the gateway, the kit-resolved
tool install hosts, and any runtime.network_allowlist declared by the
agent. Each entry is a hostname with an optional ":port" suffix.

This is the recommended fix for "Blocked by network policy" 403s on a
host the auto-installer can't infer (custom MCP endpoint, third-party
API, registry not covered by the aqua resolver):

  docker-agent sandbox allow api.example.com`,
		Args: cobra.MinimumNArgs(1),
		RunE: runSandboxAllowCommand,
	}
}

func newSandboxDenyCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "deny <host>",
		Aliases: []string{"remove", "rm"},
		Short:   "Remove a host from the persistent sandbox network allowlist",
		Args:    cobra.ExactArgs(1),
		RunE:    runSandboxDenyCommand,
	}
}

func newSandboxListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the persistent sandbox network allowlist",
		Args:    cobra.NoArgs,
		RunE:    runSandboxListCommand,
	}
}

func runSandboxAllowCommand(cmd *cobra.Command, args []string) (commandErr error) {
	// Raw hostnames may identify private corp endpoints; only count them.
	telemetry.TrackCommand(cmd.Context(), "sandbox", []string{"allow"})
	defer func() {
		telemetry.TrackCommandError(cmd.Context(), "sandbox", []string{"allow"}, commandErr)
	}()

	out := cli.NewPrinter(cmd.OutOrStdout())

	var added []string
	if err := userconfig.Update(func(cfg *userconfig.Config) error {
		var err error
		added, err = cfg.AddSandboxHosts(args...)
		return err
	}); err != nil {
		return err
	}

	if len(added) == 0 {
		out.Println("All requested hosts were already on the allowlist.")
		return nil
	}
	out.Printf("Added %d host(s) to the persistent sandbox allowlist:\n", len(added))
	for _, h := range added {
		out.Printf("  + %s\n", h)
	}
	if skipped := len(args) - len(added); skipped > 0 {
		out.Printf("(%d already present)\n", skipped)
	}
	return nil
}

func runSandboxDenyCommand(cmd *cobra.Command, args []string) (commandErr error) {
	telemetry.TrackCommand(cmd.Context(), "sandbox", []string{"deny"})
	defer func() {
		telemetry.TrackCommandError(cmd.Context(), "sandbox", []string{"deny"}, commandErr)
	}()

	out := cli.NewPrinter(cmd.OutOrStdout())
	host := strings.TrimSpace(args[0])

	if err := userconfig.Update(func(cfg *userconfig.Config) error {
		if !cfg.RemoveSandboxHost(host) {
			return errSandboxHostAbsent
		}
		return nil
	}); err != nil {
		if errors.Is(err, errSandboxHostAbsent) {
			// Idempotent: removing an already-absent host is a no-op so
			// scripts can `sandbox deny <host>` without first checking.
			out.Printf("Host %q is not on the persistent sandbox allowlist.\n", host)
			return nil
		}
		return err
	}
	out.Printf("Removed %s from the persistent sandbox allowlist.\n", host)
	return nil
}

func runSandboxListCommand(cmd *cobra.Command, args []string) (commandErr error) {
	telemetry.TrackCommand(cmd.Context(), "sandbox", []string{"list"})
	defer func() {
		telemetry.TrackCommandError(cmd.Context(), "sandbox", []string{"list"}, commandErr)
	}()

	out := cli.NewPrinter(cmd.OutOrStdout())

	cfg, err := userconfig.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if len(cfg.SandboxAllowlist) == 0 {
		out.Println("Persistent sandbox allowlist is empty.")
		out.Printf("\nAdd a host with: %s sandbox allow <host>\n", invocation.DockerAgent())
		return nil
	}

	out.Printf("Persistent sandbox allowlist (%d):\n\n", len(cfg.SandboxAllowlist))
	for _, h := range cfg.SandboxAllowlist {
		out.Printf("  %s\n", h)
	}
	return nil
}
