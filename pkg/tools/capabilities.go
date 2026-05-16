package tools

import (
	"context"

	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

// Startable is implemented by toolsets that require initialization before use.
// Toolsets that don't implement this interface are assumed to be ready immediately.
type Startable interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Statable is implemented by toolsets that expose a lifecycle state
// snapshot (Stopped/Starting/Ready/Degraded/Restarting/Failed) plus the
// most recent error and restart count. The TUI uses this to render
// the /tools dialog without polling each transport individually.
//
// Toolsets that do not implement Statable are reported as "unknown" by
// status surfaces.
type Statable interface {
	State() lifecycle.StateInfo
}

// Restartable is implemented by toolsets that can be restarted in place
// (typically the supervisor-backed MCP and LSP toolsets). Restart closes
// the active session and waits for the supervisor to bring up a fresh one,
// or returns an error on timeout.
//
// The expected use case is post-OAuth recovery ("I just authenticated,
// reconnect this MCP") and operator-driven debugging through the
// /toolset-restart slash command.
type Restartable interface {
	Restart(ctx context.Context) error
}

// Instructable is implemented by toolsets that provide custom instructions.
type Instructable interface {
	Instructions() string
}

// Elicitable is implemented by toolsets that support MCP elicitation.
type Elicitable interface {
	SetElicitationHandler(handler ElicitationHandler)
}

// OAuthCapable is implemented by toolsets that support OAuth flows.
type OAuthCapable interface {
	SetOAuthSuccessHandler(handler func())
	SetManagedOAuth(managed bool)
}

// GetInstructions returns instructions if the toolset implements Instructable.
// Returns empty string if the toolset doesn't provide instructions.
func GetInstructions(ts ToolSet) string {
	if i, ok := As[Instructable](ts); ok {
		return i.Instructions()
	}
	return ""
}

// ChangeNotifier is implemented by toolsets that can notify when their
// tool list changes (e.g. after an MCP ToolListChanged notification).
type ChangeNotifier interface {
	SetToolsChangedHandler(handler func())
}

// ConfigureHandlers sets all applicable handlers on a toolset.
// It checks for Elicitable and OAuthCapable interfaces and configures them.
// This is a convenience function that handles the capability checking internally.
func ConfigureHandlers(ts ToolSet, elicitHandler ElicitationHandler, oauthHandler func(), managedOAuth bool) {
	if e, ok := As[Elicitable](ts); ok {
		e.SetElicitationHandler(elicitHandler)
	}
	if o, ok := As[OAuthCapable](ts); ok {
		o.SetOAuthSuccessHandler(oauthHandler)
		o.SetManagedOAuth(managedOAuth)
	}
}
