package todotool

import (
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// New creates a new unified todo component.
// This component handles create, create_multiple, list, and update operations.
// The TODOs themselves are displayed in the sidebar; here we only show the
// tool call header (icon + name).
func New(msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	return toolcommon.NewBase(msg, sessionState, toolcommon.NoArgsRenderer)
}
