package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

func TestPersistenceObserver_SubSessionExistingRow(t *testing.T) {
	t.Parallel()

	for _, canceled := range []bool{false, true} {
		name := "completed"
		if canceled {
			name = "canceled"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			store, err := sqlitestore.New(ctx, filepath.Join(t.TempDir(), "sessions.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			parent := session.New(session.WithID("parent"))
			observer := newPersistenceObserver(store)
			observer.OnRunStart(ctx, parent)
			child := session.New(session.WithParentID(parent.ID), session.WithUserMessage("task"))
			// Runtime metadata writes can create the child row before completion.
			require.NoError(t, store.UpdateSession(ctx, child))
			child.AddMessage(session.NewAgentMessage("worker", &chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "finished",
				Usage:   &chat.Usage{InputTokens: 100, OutputTokens: 20},
				Cost:    0.25,
			}))
			child.SetTokensAndCost(100, 20, 0.25)
			eventCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			if canceled {
				cancel()
			}
			observer.OnEvent(eventCtx, parent, SubSessionCompleted(parent.ID, child, "root"))

			loaded, err := store.GetSession(ctx, parent.ID)
			require.NoError(t, err)
			require.Len(t, loaded.Messages, 1)
			got := loaded.Messages[0].SubSession
			require.NotNil(t, got)
			assert.Equal(t, child.ID, got.ID)
			assert.Equal(t, "finished", got.GetLastAssistantMessageContent())
			input, output, cost := got.TokensAndCost()
			assert.Equal(t, int64(100), input)
			assert.Equal(t, int64(20), output)
			assert.InDelta(t, 0.25, cost, 1e-9)
		})
	}
}
