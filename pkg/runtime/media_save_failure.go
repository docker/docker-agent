package runtime

import (
	"errors"
	"io/fs"
	"syscall"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/workspacemedia"
)

// retryWithDebugAdvice is the user-facing fallback for a generated-media
// save failure with no safe classified reason: it tells the user how to
// capture the technical details instead of leaking any of them.
const retryWithDebugAdvice = "Enable --debug and retry to capture technical details."

// mediaSaveFailureReason maps a generated-media save failure to a fixed,
// user-safe sentence for the runtime WarningEvent. Every return value is a
// constant: nothing from err — which may embed the absolute workspace
// root, a requested path, a session ID, or raw OS error text — ever
// reaches the warning. The detailed error belongs in the debug log only.
func mediaSaveFailureReason(err error) string {
	switch {
	case errors.Is(err, session.ErrWorkingDirUnavailable):
		return "No session workspace is available to save into."
	case errors.Is(err, workspacemedia.ErrNameExhausted):
		return "Every candidate filename is already taken."
	case errors.Is(err, workspacemedia.ErrPathEscape):
		return "The requested save path was refused."
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EROFS):
		return "The save location is not writable."
	case errors.Is(err, fs.ErrNotExist):
		return "The save location no longer exists."
	default:
		return retryWithDebugAdvice
	}
}
