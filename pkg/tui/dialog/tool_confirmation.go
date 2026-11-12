package dialog

import (
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	tooldefaults "github.com/docker/docker-agent/pkg/tui/components/tool/defaults"
	"github.com/docker/docker-agent/pkg/tui/dialog/toolconfirmation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

type (
	RuntimeResumeMsg         = toolconfirmation.RuntimeResumeMsg
	ToolConfirmationResponse = toolconfirmation.Response
	ConfirmationSessionState = toolconfirmation.ConfirmationSessionState
)

var (
	_ ConfirmationSessionState = (*service.SessionState)(nil)
	_ ConfirmationSessionState = (*service.EmbeddedSessionState)(nil)
)

func NewToolConfirmationDialog(ar *animation.Runtime, msg *runtime.ToolCallConfirmationEvent, sessionState ConfirmationSessionState, registries ...*tool.Registry) Dialog {
	registry := tooldefaults.NewRegistry()
	if len(registries) > 0 && registries[0] != nil {
		registry = registries[0]
	}
	return toolconfirmation.NewToolConfirmationDialog(ar, msg, sessionState, messages.WithToolRenderers(registry))
}
