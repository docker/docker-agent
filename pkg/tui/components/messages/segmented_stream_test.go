package messages

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestVisibleStreamRetainsStableLinesWithoutTranscriptCopies(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
	m.SetSize(80, 12)
	msg := types.Agent(types.MessageTypeAssistant, "root", "start ")
	m.messages = append(m.messages, msg)
	m.views = append(m.views, m.createMessageView(msg))
	_ = m.View()

	for i := range 200 {
		chunk := fmt.Sprintf("chunk-%03d **bold** ", i)
		if i%8 == 7 {
			chunk += "\n\n"
		}
		m.AppendToLastMessage("root", chunk)
		_ = m.View()
	}
	require.NotNil(t, m.activeSegments)
	require.Greater(t, len(m.activeSegments.stable), 20)

	stableLen := len(m.activeSegments.stable)
	for i := range 100 {
		chunk := fmt.Sprintf("late-%03d `code` ", i)
		if i%8 == 7 {
			chunk += "\n\n"
		}
		m.AppendToLastMessage("root", chunk)
		_ = m.View()
	}
	require.NotNil(t, m.activeSegments)
	require.GreaterOrEqual(t, len(m.activeSegments.stable), stableLen)
	require.LessOrEqual(t, len(m.activeSegments.tail), 32, "mutable suffix remains structurally bounded")
}
