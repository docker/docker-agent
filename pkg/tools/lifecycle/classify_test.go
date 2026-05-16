package lifecycle_test

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"testing"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"

	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

func TestClassify_Nil(t *testing.T) {
	t.Parallel()
	assert.Check(t, is.Nil(lifecycle.Classify(nil)))
}

func TestClassify_ExecNotFound(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("start command: %w", exec.ErrNotFound)
	got := lifecycle.Classify(wrapped)
	assert.Check(t, errors.Is(got, lifecycle.ErrServerUnavailable))
	assert.Check(t, errors.Is(got, exec.ErrNotFound))
}

func TestClassify_FileNotExist(t *testing.T) {
	t.Parallel()
	got := lifecycle.Classify(fmt.Errorf("%w", os.ErrNotExist))
	assert.Check(t, errors.Is(got, lifecycle.ErrServerUnavailable))
}

func TestClassify_EOF(t *testing.T) {
	t.Parallel()
	got := lifecycle.Classify(io.EOF)
	assert.Check(t, errors.Is(got, lifecycle.ErrServerUnavailable))
}

func TestClassify_NetError(t *testing.T) {
	t.Parallel()
	// *net.OpError satisfies net.Error.
	netErr := &net.OpError{Op: "dial", Err: errors.New("refused")}
	got := lifecycle.Classify(netErr)
	assert.Check(t, errors.Is(got, lifecycle.ErrTransport))
}

func TestClassify_InitNotification(t *testing.T) {
	t.Parallel()
	got := lifecycle.Classify(errors.New("failed to send initialized notification: bad"))
	assert.Check(t, errors.Is(got, lifecycle.ErrInitNotification))
}

func TestClassify_SessionMissingByMessage(t *testing.T) {
	t.Parallel()
	got := lifecycle.Classify(errors.New("session not found"))
	assert.Check(t, errors.Is(got, lifecycle.ErrSessionMissing))
}

func TestClassify_TransportByMessage(t *testing.T) {
	t.Parallel()
	cases := []string{
		"connection reset by peer",
		"connection refused",
		"write tcp ...: broken pipe",
		"unexpected EOF",
	}
	for _, msg := range cases {
		got := lifecycle.Classify(errors.New(msg))
		assert.Check(t, errors.Is(got, lifecycle.ErrTransport), "msg=%q", msg)
	}
}

func TestClassify_AlreadyClassifiedPasses(t *testing.T) {
	t.Parallel()
	in := fmt.Errorf("wrapped: %w", lifecycle.ErrAuthRequired)
	got := lifecycle.Classify(in)
	assert.Check(t, errors.Is(got, lifecycle.ErrAuthRequired))
}

func TestClassify_UnknownPassthrough(t *testing.T) {
	t.Parallel()
	in := errors.New("totally unrelated")
	got := lifecycle.Classify(in)
	assert.Check(t, is.Equal(got, in))
}

func TestClassified_ErrorMessageIncludesBoth(t *testing.T) {
	t.Parallel()
	got := lifecycle.Classify(io.EOF)
	assert.Check(t, is.Contains(got.Error(), "server unavailable"))
	assert.Check(t, is.Contains(got.Error(), "EOF"))
}

func TestIsTransient(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"transport", lifecycle.ErrTransport, true},
		{"unavailable", lifecycle.ErrServerUnavailable, true},
		{"crashed", lifecycle.ErrServerCrashed, true},
		{"init-timeout", lifecycle.ErrInitTimeout, true},
		{"init-notif", lifecycle.ErrInitNotification, true},
		{"session", lifecycle.ErrSessionMissing, true},
		{"capability", lifecycle.ErrCapabilityMissing, false},
		{"auth", lifecycle.ErrAuthRequired, false},
		{"nil", nil, false},
		{"unknown", errors.New("x"), false},
	}
	for _, tc := range cases {
		got := lifecycle.IsTransient(tc.err)
		assert.Check(t, is.Equal(got, tc.want), "case=%s", tc.name)
	}
}

func TestIsPermanent(t *testing.T) {
	t.Parallel()
	assert.Check(t, lifecycle.IsPermanent(lifecycle.ErrAuthRequired))
	assert.Check(t, lifecycle.IsPermanent(lifecycle.ErrCapabilityMissing))
	assert.Check(t, !lifecycle.IsPermanent(lifecycle.ErrTransport))
	assert.Check(t, !lifecycle.IsPermanent(nil))
}
