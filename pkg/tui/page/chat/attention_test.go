package chat

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestAttentionDialogOwnership(t *testing.T) {
	t.Parallel()
	for _, hosted := range []bool{false, true} {
		name := "standalone"
		if hosted {
			name = "hosted"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sess := session.New()
			p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess))
			t.Cleanup(func() { Cleanup(p) })
			if hosted {
				p.SetRoutingID("tab")
			}
			for _, event := range []tea.Msg{
				runtime.ToolCallConfirmation(tools.ToolCall{ID: "tool"}, tools.Tool{Name: "shell"}, "root", nil),
				&runtime.MaxIterationsReachedEvent{MaxIterations: 10},
				&runtime.ElicitationRequestEvent{ElicitationID: "form"},
				&runtime.ElicitationRequestEvent{ElicitationID: "url", Mode: "url", URL: "https://example.com"},
				&runtime.ElicitationRequestEvent{ElicitationID: "oauth", Meta: map[string]any{"docker-agent/type": "oauth_flow"}},
			} {
				_, effects := p.UpdateEffects(event)
				var opened []dialog.OpenDialogMsg
				for _, msg := range runTimerCmd(t, effects.Cmd(true)) {
					if open, ok := msg.(dialog.OpenDialogMsg); ok {
						opened = append(opened, open)
					}
				}
				if hosted {
					assert.Empty(t, opened)
				} else {
					require.Len(t, opened, 1)
					assert.Same(t, event, opened[0].OriginatingEvent)
				}
			}
		})
	}
}
