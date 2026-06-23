package root

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/tools/builtin/shell"
)

// newAskpassCmd returns the hidden `__askpass` helper that sudo invokes (via a
// generated SUDO_ASKPASS wrapper script) when the shell toolset's
// `sudo_askpass` flow is enabled. It bridges the password prompt back to the
// running agent over a private unix socket and prints the password to stdout.
// It is an internal helper, not meant to be run by hand.
func newAskpassCmd() *cobra.Command {
	return &cobra.Command{
		Use:    shell.AskpassCommandName + " [prompt]",
		Short:  "Internal sudo askpass helper",
		Args:   cobra.MaximumNArgs(1),
		Hidden: true,
		// Keep stdout clean: sudo reads the password from this command's
		// stdout, so nothing else may write there.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var prompt string
			if len(args) > 0 {
				prompt = args[0]
			}
			if err := shell.RunAskpassClient(cmd.Context(), prompt, cmd.OutOrStdout()); err != nil {
				// Diagnostics go to stderr only; stdout must carry just the
				// password for sudo to read.
				fmt.Fprintln(cmd.ErrOrStderr(), "docker-agent askpass:", err)
				return err
			}
			return nil
		},
	}
}
