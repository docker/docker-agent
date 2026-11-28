package runtime

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

func TestPersistenceObserverKeepsCostWhenSessionIsStored(t *testing.T) {
	t.Parallel()

	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "session.db"))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close()) })

	sess := session.New(session.WithUserMessage("hi"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	obs := newPersistenceObserver(store)

	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "answer", Cost: 0.5}))
	obs.OnEvent(t.Context(), sess, NewTokenUsageEvent(sess.ID, "root", SessionUsage(sess, 0)))
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	row, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	_, _, cost := row.TokensAndCost()
	assert.InDelta(t, 0.5, cost, 1e-9)
}
