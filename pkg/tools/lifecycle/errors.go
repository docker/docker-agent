// Package lifecycle defines the shared vocabulary used by long-running
// toolsets (MCP servers, remote MCP, LSP servers): typed error sentinels,
// a State enum, a Tracker, and a Supervisor that drives a Connector
// through connect / watch / restart / stop.
package lifecycle

import "errors"

// Sentinel errors used to classify failures across MCP and LSP transports.
//
// Concrete transports wrap their underlying SDK errors with these (via
// Classify) so supervisors can decide policy via errors.Is rather than
// substring matching. New error categories should be added here rather
// than as ad-hoc strings.
var (
	// ErrTransport is a transport-level failure (connection lost or never
	// established). Usually restartable.
	ErrTransport = errors.New("transport failure")

	// ErrServerUnavailable means the server could not be reached at all
	// (binary missing, immediate EOF on stdin, connection refused).
	// Restartable on a slower cadence.
	ErrServerUnavailable = errors.New("server unavailable")

	// ErrServerCrashed means the process started but exited unexpectedly.
	// Restartable per policy.
	ErrServerCrashed = errors.New("server crashed")

	// ErrInitTimeout means the initialize handshake did not complete
	// within the configured deadline.
	ErrInitTimeout = errors.New("initialize timed out")

	// ErrInitNotification means the server accepted initialize but the
	// client failed to send the followup "initialized" notification.
	// Retryable transient documented upstream.
	ErrInitNotification = errors.New("failed to send initialized notification")

	// ErrCapabilityMissing means the server doesn't advertise a required
	// capability. Restarting won't help; supervisor should not retry.
	ErrCapabilityMissing = errors.New("capability not supported")

	// ErrAuthRequired means OAuth (or similar) is required. Supervisor
	// should park, not loop; resumption happens after the user authenticates.
	ErrAuthRequired = errors.New("authentication required")

	// ErrSessionMissing means the server lost the client's session
	// (e.g. a remote MCP server restarted). Force a reconnect.
	ErrSessionMissing = errors.New("session missing")

	// ErrNotStarted means an operation was attempted on a toolset that has
	// not yet successfully started.
	ErrNotStarted = errors.New("toolset not started")
)
