package dialog

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

type recordingElicitationResponder struct {
	calls      int
	action     tools.ElicitationAction
	id         string
	contextErr error
	value      any
}

func (r *recordingElicitationResponder) ResumeElicitation(ctx context.Context, action tools.ElicitationAction, content map[string]any, ids ...string) error {
	r.calls++
	r.action = action
	r.id = firstElicitationID(ids)
	r.contextErr = ctx.Err()
	r.value = ctx.Value(oauthContextKey{})
	return errors.New("response failed")
}

type oauthContextKey struct{}

func TestOAuthAuthorizationResponder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		key    rune
		action tools.ElicitationAction
	}{
		{'y', tools.ElicitationActionAccept},
		{'n', tools.ElicitationActionDecline},
	} {
		t.Run(string(tc.key), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), oauthContextKey{}, "retained"))
			cancel()
			responder := &recordingElicitationResponder{}
			d := NewAttentionDialog(ctx, nil, responder, nil, &runtime.ElicitationRequestEvent{
				ElicitationID: "request-1",
				Meta:          map[string]any{"docker-agent/type": "oauth_flow", "docker-agent/server_url": "https://example.com"},
			})
			require.NotNil(t, d)
			_, cmd := d.Update(tea.KeyPressMsg{Code: tc.key, Text: string(tc.key)})
			require.NotNil(t, cmd)
			assert.Equal(t, 1, responder.calls)
			assert.Equal(t, tc.action, responder.action)
			assert.Equal(t, "request-1", responder.id)
			require.NoError(t, responder.contextErr)
			assert.Equal(t, "retained", responder.value)
			assert.Equal(t, CloseDialogMsg{ElicitationID: "request-1"}, cmd())
		})
	}
}

func TestMaxIterationsNeedsNoApplication(t *testing.T) {
	t.Parallel()
	d := NewAttentionDialog(t.Context(), nil, nil, nil, &runtime.MaxIterationsReachedEvent{MaxIterations: 10})
	require.IsType(t, &maxIterationsDialog{}, d)
	assert.Equal(t, 10, d.(*maxIterationsDialog).maxIterations)
	_, cmd := d.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	require.NotNil(t, cmd)
}
