package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoresUpdateMessageIsScopedToSession(t *testing.T) {
	for _, storeFactory := range []struct {
		name string
		new  func(*testing.T) Store
	}{
		{name: "memory", new: func(t *testing.T) Store {
			t.Helper()
			return NewInMemorySessionStore()
		}},
		{name: "sqlite", new: func(t *testing.T) Store {
			t.Helper()
			store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "sessions.db"))
			require.NoError(t, err)
			return store
		}},
	} {
		t.Run(storeFactory.name, func(t *testing.T) {
			ctx := t.Context()
			store := storeFactory.new(t)
			t.Cleanup(func() { require.NoError(t, store.Close()) })

			owner := New()
			other := New()
			require.NoError(t, store.AddSession(ctx, owner))
			require.NoError(t, store.AddSession(ctx, other))
			messageID, err := store.AddMessage(ctx, owner.ID, UserMessage("original"))
			require.NoError(t, err)

			err = store.UpdateMessage(ctx, other.ID, messageID, UserMessage("wrong session"))
			require.ErrorIs(t, err, ErrNotFound)
			stored, err := store.GetSession(ctx, owner.ID)
			require.NoError(t, err)
			require.Len(t, stored.GetAllMessages(), 1)
			assert.Equal(t, "original", stored.GetAllMessages()[0].Message.Content)

			require.NoError(t, store.UpdateMessage(ctx, owner.ID, messageID, UserMessage("updated")))
			stored, err = store.GetSession(ctx, owner.ID)
			require.NoError(t, err)
			require.Len(t, stored.GetAllMessages(), 1)
			assert.Equal(t, "updated", stored.GetAllMessages()[0].Message.Content)
		})
	}
}
