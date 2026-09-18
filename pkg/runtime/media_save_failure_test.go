package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/workspacemedia"
)

// TestMediaSaveFailureReason proves every classified failure maps to its
// fixed sentence, that anything unclassified falls back to the
// retry-with-debug advice, and — the redaction contract — that no fragment
// of the underlying error (absolute paths, session IDs, raw OS error text)
// ever survives into the returned reason.
func TestMediaSaveFailureReason(t *testing.T) {
	t.Parallel()

	const (
		secretRoot = "/Users/someone/secret-workspace"
		sessionID  = "sess-1234-secret"
	)

	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "no workspace provenance",
			err:  fmt.Errorf("%w: session %s has no workspace root and no parent", session.ErrWorkingDirUnavailable, sessionID),
			want: "No session workspace is available to save into.",
		},
		{
			name: "filename collision exhaustion",
			err:  fmt.Errorf("%w: %q after 10000 attempts", workspacemedia.ErrNameExhausted, secretRoot+"/cat.png"),
			want: "Every candidate filename is already taken.",
		},
		{
			name: "requested path refused",
			err:  fmt.Errorf("%w: %q: absolute path", workspacemedia.ErrPathEscape, secretRoot),
			want: "The requested save path was refused.",
		},
		{
			name: "permission denied",
			err:  &fs.PathError{Op: "open", Path: secretRoot, Err: fs.ErrPermission},
			want: "The save location is not writable.",
		},
		{
			name: "read-only filesystem",
			err:  &fs.PathError{Op: "open", Path: secretRoot, Err: syscall.EROFS},
			want: "The save location is not writable.",
		},
		{
			name: "workspace root gone",
			err:  fmt.Errorf("open workspace root: %w", &fs.PathError{Op: "open", Path: secretRoot, Err: fs.ErrNotExist}),
			want: "The save location no longer exists.",
		},
		{
			name: "unclassified error",
			err:  errors.New("write " + secretRoot + "/tmp-1: device timeout for " + sessionID),
			want: retryWithDebugAdvice,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := mediaSaveFailureReason(tc.err)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, secretRoot, "the reason must never echo a path from the error")
			assert.NotContains(t, got, sessionID, "the reason must never echo a session ID from the error")
			assert.NotContains(t, got, tc.err.Error(), "the reason must never echo the raw error text")
		})
	}
}
