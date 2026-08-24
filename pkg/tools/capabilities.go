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

// StartReporter is implemented by toolsets whose live lifecycle state can
// be queried independently of the StartableToolSet wrapper's latched state
// (e.g. an MCP toolset whose supervisor lost the session in the background).
// The wrapper consults it on Start to decide whether a recovery is needed,
// and composite toolsets consult their inner toolsets' reporters to detect
// an inner that started successfully and later died.
type StartReporter interface {
	IsStarted() bool
}

// PeerDependent is implemented by toolsets whose Start reads from the
// agent's other toolsets (e.g. the deferred aggregator lists its source
// toolsets' tools). Callers that start an agent's toolsets concurrently
// must start these only after every other toolset has settled, or their
// Start would race the very toolsets it depends on.
type PeerDependent interface {
	StartsAfterPeers()
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

// Sampleable is implemented by toolsets that support MCP sampling
// (sampling/createMessage). MCP servers use sampling to delegate LLM calls
// back to the host; the handler is expected to drive the host's model.
type Sampleable interface {
	SetSamplingHandler(handler SamplingHandler)
}

// SampleableWithTools is implemented by toolsets that support MCP sampling
// requests carrying a tools array (sampling-with-tools). The handler is
// invoked instead of the basic SamplingHandler when both are registered and
// the SDK negotiates the with-tools variant on the wire.
type SampleableWithTools interface {
	SetSamplingWithToolsHandler(handler SamplingWithToolsHandler)
}

// OAuthCapable is implemented by toolsets that support OAuth flows.
type OAuthCapable interface {
	SetOAuthSuccessHandler(handler func())
	SetManagedOAuth(managed bool)
	// SetUnmanagedOAuthRedirectURI sets the `redirect_uri` that docker-agent
	// advertises when running an MCP server OAuth flow in unmanaged mode.
	// When non-empty, docker-agent drives PKCE + DCR + token exchange itself
	// and expects the client to return {code, state} (in addition to the
	// existing {access_token, …} reply shape). Ignored in managed mode.
	SetUnmanagedOAuthRedirectURI(uri string)
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
// It checks for Elicitable, Sampleable, SampleableWithTools, and OAuthCapable
// interfaces and configures them. This is a convenience function that handles
// the capability checking internally.
func ConfigureHandlers(ts ToolSet, elicitHandler ElicitationHandler, samplingHandler SamplingHandler, samplingWithToolsHandler SamplingWithToolsHandler, oauthHandler func(), managedOAuth bool, unmanagedOAuthRedirectURI string) {
	if e, ok := As[Elicitable](ts); ok {
		e.SetElicitationHandler(elicitHandler)
	}
	if s, ok := As[Sampleable](ts); ok {
		s.SetSamplingHandler(samplingHandler)
	}
	if s, ok := As[SampleableWithTools](ts); ok {
		s.SetSamplingWithToolsHandler(samplingWithToolsHandler)
	}
	if o, ok := As[OAuthCapable](ts); ok {
		o.SetOAuthSuccessHandler(oauthHandler)
		o.SetManagedOAuth(managedOAuth)
		o.SetUnmanagedOAuthRedirectURI(unmanagedOAuthRedirectURI)
	}
}
