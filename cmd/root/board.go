package root

import (
	"github.com/spf13/cobra"

	boardtui "github.com/docker/docker-agent/pkg/board/tui"
	"github.com/docker/docker-agent/pkg/telemetry"
)

// newBoardCmd creates the `docker agent board` command: a Kanban TUI that
// orchestrates one agent per card, each running in a tmux session on an
// isolated git worktree. Projects and column prompts are configured in the
// user's global config file, or from the TUI itself.
func newBoardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "board",
		Short: "Orchestrate agents on a Kanban board",
		Long: `Board is a Kanban TUI for orchestrating agents. Each card launches an agent
in a tmux session on an isolated git worktree, and moving a card forward
through the pipeline (Dev → Review → Push → Done) sends the
destination column's prompt to its agent.

Projects and column prompts are stored in the global config file
(config.yaml in the config directory; see --config-dir) and can be managed
from the TUI.`,
		Example:      `  docker-agent board`,
		Args:         cobra.NoArgs,
		GroupID:      "advanced",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			telemetry.TrackCommand(cmd.Context(), "board", nil)
			applyTheme("")
			return boardtui.Run(cmd.Context())
		},
	}
	return cmd
}
