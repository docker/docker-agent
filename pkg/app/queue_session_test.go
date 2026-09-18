package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type queueSessionRuntime struct {
	mockRuntime

	submitted []runtime.QueuedMessage
}

func (r *queueSessionRuntime) Steer(_ context.Context, msg runtime.QueuedMessage) error {
	r.submitted = append(r.submitted, msg)
	return nil
}

func (r *queueSessionRuntime) FollowUp(_ context.Context, msg runtime.QueuedMessage) error {
	r.submitted = append(r.submitted, msg)
	return nil
}

func TestQueueMessageUsesSessionSnapshot(t *testing.T) {
	t.Parallel()
	for _, follow := range []bool{false, true} {
		t.Run(map[bool]string{false: "steer", true: "follow-up"}[follow], func(t *testing.T) {
			t.Parallel()
			rt := &queueSessionRuntime{}
			original := session.New()
			a := New(t.Context(), rt, original)
			enqueue := a.QueueSteerMessageForSession
			if follow {
				enqueue = a.QueueFollowUpMessageForSession
			}
			replacement := session.New()
			a.ReplaceSession(t.Context(), replacement)
			file := filepath.Join(t.TempDir(), "note.txt")
			require.NoError(t, os.WriteFile(file, []byte("original attachment"), 0o600))
			submitted, err := enqueue(t.Context(), original, "hello", []messages.Attachment{{FilePath: file}})
			require.NoError(t, err)
			assert.Equal(t, []string{file}, original.AttachedFilesSnapshot())
			assert.Empty(t, replacement.AttachedFilesSnapshot())
			require.Len(t, rt.submitted, 1)
			assert.Equal(t, submitted, rt.submitted[0])
			assert.NotEmpty(t, submitted.ID)
			assert.NotEmpty(t, submitted.MultiContent)
		})
	}
}

func TestQueueMessageRejectsCancelledSubmission(t *testing.T) {
	t.Parallel()
	for _, follow := range []bool{false, true} {
		t.Run(map[bool]string{false: "steer", true: "follow-up"}[follow], func(t *testing.T) {
			t.Parallel()
			rt := &queueSessionRuntime{}
			sess := session.New()
			a := New(t.Context(), rt, sess)
			enqueue := a.QueueSteerMessageForSession
			if follow {
				enqueue = a.QueueFollowUpMessageForSession
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err := enqueue(ctx, sess, "cancelled", nil)
			require.ErrorIs(t, err, context.Canceled)
			assert.Empty(t, rt.submitted)
		})
	}
}
