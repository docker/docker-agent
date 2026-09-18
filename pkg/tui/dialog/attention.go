package dialog

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// NewAttentionDialog builds a prompt without opening it or running its Init command.
func NewAttentionDialog(ctx context.Context, ar *animation.Runtime, application *app.App, state *service.SessionState, event tea.Msg) Dialog {
	switch ev := event.(type) {
	case *runtime.ToolCallConfirmationEvent:
		return NewToolConfirmationDialog(ar, ev, state)
	case *runtime.MaxIterationsReachedEvent:
		return NewMaxIterationsDialog(ev.MaxIterations, application)
	case *runtime.ElicitationRequestEvent:
		if ev.Meta["docker-agent/type"] == "oauth_flow" {
			serverURL, _ := ev.Meta["docker-agent/server_url"].(string)
			return NewOAuthAuthorizationDialog(ctx, serverURL, application, ev.ElicitationID)
		}
		if ev.Mode == "url" {
			return NewURLElicitationDialog(ctx, ev.Message, ev.URL, ev.ElicitationID)
		}
		return NewElicitationDialog(ev.Message, ev.Schema, ev.Meta, ev.ElicitationID)
	default:
		return nil
	}
}
