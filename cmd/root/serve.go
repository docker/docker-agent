package root

import (
	"github.com/spf13/cobra"
)

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "serve",
		Short:   "Start an agent as a server",
		GroupID: "advanced",
	}

	cmd.AddCommand(newA2ACmd())
	cmd.AddCommand(newACPCmd())
	cmd.AddCommand(newAPICmd())
	cmd.AddCommand(newChatCmd())
	cmd.AddCommand(newMCPCmd())

	return cmd
}
