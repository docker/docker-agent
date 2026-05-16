package root

import "github.com/spf13/cobra"

func newShareCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "share",
		Short:   "Share agents",
		GroupID: "core",
	}

	cmd.AddCommand(newPushCmd())
	cmd.AddCommand(newPullCmd())

	return cmd
}
