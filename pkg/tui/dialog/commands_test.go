package dialog

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/dialog/internal/testutil"
)

func collectMsgs(cmd tea.Cmd) []tea.Msg {
	return testutil.CollectMsgs(cmd)
}
