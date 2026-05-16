package session

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
)

func TestStoreAgentName(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "test_store.db")

	store, err := NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	testAgent1 := agent.New("test-agent-1", "test prompt 1")
	testAgent2 := agent.New("test-agent-2", "test prompt 2")

	session := &Session{
		ID: "test-session",
		Messages: []Item{
			NewMessageItem(UserMessage("Hello")),
			NewMessageItem(NewAgentMessage(testAgent1.Name(), &chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "Hello from test-agent-1",
			})),
			NewMessageItem(NewAgentMessage(testAgent2.Name(), &chat.Message{
				Role:    chat.MessageRoleUser,
				Content: "Another message from test-agent-2",
			})),
		},
		InputTokens:  100,
		OutputTokens: 200,
		CreatedAt:    time.Now(),
	}

	// Store the session
	err = store.AddSession(t.Context(), session)
	require.NoError(t, err)

	// Retrieve the session
	retrievedSession, err := store.GetSession(t.Context(), "test-session")
	require.NoError(t, err)
	require.NotNil(t, retrievedSession)

	assert.Len(t, retrievedSession.GetAllMessages(), 3)

	// First message should be user message with empty agent name
	assert.Empty(t, retrievedSession.Messages[0].Message.AgentName)
	assert.Equal(t, "Hello", retrievedSession.Messages[0].Message.Message.Content)

	// Second message should have the first agent's name
	assert.Equal(t, "test-agent-1", retrievedSession.Messages[1].Message.AgentName)
	assert.Equal(t, "Hello from test-agent-1", retrievedSession.Messages[1].Message.Message.Content)

	// Third message should have the second agent's name
	assert.Equal(t, "test-agent-2", retrievedSession.Messages[2].Message.AgentName)
	assert.Equal(t, "Another message from test-agent-2", retrievedSession.Messages[2].Message.Message.Content)
}

func TestStoreMultipleAgents(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "test_store_multi.db")

	store, err := NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	agent1 := agent.New("agent-1", "agent 1 prompt")
	agent2 := agent.New("agent-2", "agent 2 prompt")

	session := &Session{
		ID:        "multi-agent-session",
		CreatedAt: time.Now(),
		Messages: []Item{
			NewMessageItem(UserMessage("Start conversation")),
			NewMessageItem(NewAgentMessage(agent1.Name(), &chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "Response from agent 1",
			})),
			NewMessageItem(NewAgentMessage(agent2.Name(), &chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "Response from agent 2",
			})),
		},
	}

	// Store the session
	err = store.AddSession(t.Context(), session)
	require.NoError(t, err)

	// Retrieve the session
	retrievedSession, err := store.GetSession(t.Context(), "multi-agent-session")
	require.NoError(t, err)
	require.NotNil(t, retrievedSession)

	assert.Len(t, retrievedSession.Messages, 3)

	// First message should be user message with empty agent name
	assert.Empty(t, retrievedSession.Messages[0].Message.AgentName)

	// Second message should have agent-1 name
	assert.Equal(t, "agent-1", retrievedSession.Messages[1].Message.AgentName)
	assert.Equal(t, "Response from agent 1", retrievedSession.Messages[1].Message.Message.Content)

	// Third message should have agent-2 name
	assert.Equal(t, "agent-2", retrievedSession.Messages[2].Message.AgentName)
	assert.Equal(t, "Response from agent 2", retrievedSession.Messages[2].Message.Message.Content)
}

func TestGetSessions(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "test_get_sessions.db")

	store, err := NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	testAgent := agent.New("test-agent", "test prompt")

	session1 := &Session{
		ID: "session-1",
		Messages: []Item{
			NewMessageItem(NewAgentMessage(testAgent.Name(), &chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "Message from session 1",
			})),
		},
		CreatedAt: time.Now().Add(-1 * time.Hour),
	}

	session2 := &Session{
		ID: "session-2",
		Messages: []Item{
			NewMessageItem(NewAgentMessage(testAgent.Name(), &chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "Message from session 2",
			})),
		},
		CreatedAt: time.Now(),
	}

	// Store the sessions
	err = store.AddSession(t.Context(), session1)
	require.NoError(t, err)
	err = store.AddSession(t.Context(), session2)
	require.NoError(t, err)

	// Retrieve all sessions
	sessions, err := store.GetSessions(t.Context())
	require.NoError(t, err)
	assert.Len(t, sessions, 2)

	for _, session := range sessions {
		assert.Len(t, session.Messages, 1)
		assert.Equal(t, "test-agent", session.Messages[0].Message.AgentName)
	}
}

func TestGetSessionSummaries(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "test_get_session_summaries.db")

	store, err := NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	testAgent := agent.New("test-agent", "test prompt")

	session1Time := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Second)
	session2Time := time.Now().UTC().Truncate(time.Second)

	session1 := &Session{
		ID:    "session-1",
		Title: "First Session",
		Messages: []Item{
			NewMessageItem(NewAgentMessage(testAgent.Name(), &chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "A very long message that should not be loaded when getting summaries",
			})),
		},
		CreatedAt: session1Time,
	}

	session2 := &Session{
		ID:    "session-2",
		Title: "Second Session",
		Messages: []Item{
			NewMessageItem(NewAgentMessage(testAgent.Name(), &chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "Another long message that should not be loaded when getting summaries",
			})),
		},
		CreatedAt: session2Time,
	}

	// Store the sessions
	err = store.AddSession(t.Context(), session1)
	require.NoError(t, err)
	err = store.AddSession(t.Context(), session2)
	require.NoError(t, err)

	// Retrieve summaries (should be lightweight, without messages)
	summaries, err := store.GetSessionSummaries(t.Context())
	require.NoError(t, err)
	assert.Len(t, summaries, 2)

	// Summaries should be ordered by created_at DESC (most recent first)
	assert.Equal(t, "session-2", summaries[0].ID)
	assert.Equal(t, "Second Session", summaries[0].Title)
	assert.Equal(t, session2Time, summaries[0].CreatedAt)
	assert.Equal(t, 1, summaries[0].NumMessages)

	assert.Equal(t, "session-1", summaries[1].ID)
	assert.Equal(t, "First Session", summaries[1].Title)
	assert.Equal(t, session1Time, summaries[1].CreatedAt)
	assert.Equal(t, 1, summaries[1].NumMessages)
}

func TestBranchSessionCopiesPrefix(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "test_branch_prefix.db")

	store, err := NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	testAgent := agent.New("test-agent", "test prompt")
	parent := &Session{
		ID:        "parent-session",
		CreatedAt: time.Now(),
		Messages: []Item{
			NewMessageItem(UserMessage("Hello")),
			NewMessageItem(NewAgentMessage(testAgent.Name(), &chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "Response",
			})),
			NewMessageItem(UserMessage("Edited")),
		},
	}

	require.NoError(t, store.AddSession(t.Context(), parent))

	parentLoaded, err := store.GetSession(t.Context(), parent.ID)
	require.NoError(t, err)

	branched, err := BranchSession(parentLoaded, 2)
	require.NoError(t, err)

	require.NoError(t, store.AddSession(t.Context(), branched))

	loaded, err := store.GetSession(t.Context(), branched.ID)
	require.NoError(t, err)

	require.Len(t, loaded.Messages, 2)
	assert.Equal(t, "Hello", loaded.Messages[0].Message.Message.Content)
	assert.Equal(t, "Response", loaded.Messages[1].Message.Message.Content)
}

func TestBranchSessionClonesSubSession(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "test_branch_subsession.db")

	store, err := NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	subSession := &Session{
		ID:        "sub-session",
		CreatedAt: time.Now(),
		Messages: []Item{
			NewMessageItem(UserMessage("Sub message")),
		},
	}
	parent := &Session{
		ID:        "parent-session",
		CreatedAt: time.Now(),
		Messages: []Item{
			NewMessageItem(UserMessage("Start")),
			NewSubSessionItem(subSession),
			NewMessageItem(UserMessage("After")),
		},
	}

	require.NoError(t, store.AddSession(t.Context(), parent))

	parentLoaded, err := store.GetSession(t.Context(), parent.ID)
	require.NoError(t, err)

	branched, err := BranchSession(parentLoaded, 2)
	require.NoError(t, err)

	require.NoError(t, store.AddSession(t.Context(), branched))

	loaded, err := store.GetSession(t.Context(), branched.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Messages, 2)

	subItem := loaded.Messages[1]
	require.NotNil(t, subItem.SubSession)
	assert.NotEqual(t, subSession.ID, subItem.SubSession.ID)
	assert.Equal(t, loaded.ID, subItem.SubSession.ParentID)
	require.Len(t, subItem.SubSession.Messages, 1)
	assert.Equal(t, "Sub message", subItem.SubSession.Messages[0].Message.Message.Content)
}

func TestStoreAgentNameJSON(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "test_store_json.db")

	store, err := NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	agent1 := agent.New("my-agent", "test prompt")
	agent2 := agent.New("another-agent", "another prompt")

	session := &Session{
		ID: "json-test-session",
		Messages: []Item{
			NewMessageItem(UserMessage("User input")),
			NewMessageItem(NewAgentMessage(agent1.Name(), &chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "Response from my-agent",
			})),
			NewMessageItem(NewAgentMessage(agent2.Name(), &chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "Response from another-agent",
			})),
		},
		CreatedAt: time.Now(),
	}

	// Store the session
	err = store.AddSession(t.Context(), session)
	require.NoError(t, err)

	// Retrieve the session
	retrievedSession, err := store.GetSession(t.Context(), "json-test-session")
	require.NoError(t, err)
	require.NotNil(t, retrievedSession)

	assert.Empty(t, retrievedSession.Messages[0].Message.AgentName)                  // User message
	assert.Equal(t, "my-agent", retrievedSession.Messages[1].Message.AgentName)      // First agent
	assert.Equal(t, "another-agent", retrievedSession.Messages[2].Message.AgentName) // Second agent
}

func TestNewSQLiteSessionStore_DirectoryNotWritable(t *testing.T) {
	readOnlyDir := filepath.Join(t.TempDir(), "readonly")
	err := os.Mkdir(readOnlyDir, 0o555)
	require.NoError(t, err)

	_, err = NewSQLiteSessionStore(filepath.Join(readOnlyDir, "session.db"))
	require.Error(t, err)

	assert.Contains(t, err.Error(), "cannot create database")
	assert.Contains(t, err.Error(), "permission denied or file cannot be created")

	// We should surface the real "cannot create database" error directly instead of
	// running the backup+retry recovery path (which cannot fix a filesystem-level
	// problem and would only wrap the real error in a confusing "migration failed"
	// message).
	assert.NotContains(t, err.Error(), "migration failed")
}

func TestUpdateSession_LazyCreation(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "test_lazy.db")

	store, err := NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	testAgent := agent.New("test-agent", "test prompt")

	// Create a session but don't add it to the store (simulating lazy creation)
	session := &Session{
		ID:        "lazy-session",
		CreatedAt: time.Now(),
	}

	// Verify session doesn't exist yet
	_, err = store.GetSession(t.Context(), "lazy-session")
	require.ErrorIs(t, err, ErrNotFound)

	// UpdateSession creates the session (upsert) but does NOT persist messages
	// Messages must be added separately via AddMessage
	err = store.UpdateSession(t.Context(), session)
	require.NoError(t, err)

	// Session exists but has no messages yet
	retrieved, err := store.GetSession(t.Context(), "lazy-session")
	require.NoError(t, err)
	assert.Empty(t, retrieved.Messages)

	// Add messages via AddMessage (the proper way)
	_, err = store.AddMessage(t.Context(), "lazy-session", UserMessage("Hello"))
	require.NoError(t, err)

	_, err = store.AddMessage(t.Context(), "lazy-session", NewAgentMessage(testAgent.Name(), &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "Hi there!",
	}))
	require.NoError(t, err)

	// Now the session should have messages
	retrieved, err = store.GetSession(t.Context(), "lazy-session")
	require.NoError(t, err)
	assert.Len(t, retrieved.Messages, 2)
	assert.Equal(t, "Hello", retrieved.Messages[0].Message.Message.Content)
	assert.Equal(t, "Hi there!", retrieved.Messages[1].Message.Message.Content)
}

func TestUpdateSession_LazyCreation_InMemory(t *testing.T) {
	store := NewInMemorySessionStore()

	testAgent := agent.New("test-agent", "test prompt")

	// Create a session but don't add it to the store
	session := &Session{
		ID:        "lazy-session",
		CreatedAt: time.Now(),
	}

	// Verify session doesn't exist yet
	_, err := store.GetSession(t.Context(), "lazy-session")
	require.ErrorIs(t, err, ErrNotFound)

	// UpdateSession creates the session (upsert) without messages
	// Messages must be added separately via AddMessage (like SQLite behavior)
	err = store.UpdateSession(t.Context(), session)
	require.NoError(t, err)

	// Session exists but has no messages yet
	retrieved, err := store.GetSession(t.Context(), "lazy-session")
	require.NoError(t, err)
	assert.Empty(t, retrieved.Messages)

	// Add messages via AddMessage
	_, err = store.AddMessage(t.Context(), "lazy-session", UserMessage("Hello"))
	require.NoError(t, err)
	_, err = store.AddMessage(t.Context(), "lazy-session", NewAgentMessage(testAgent.Name(), &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "Hi there!",
	}))
	require.NoError(t, err)

	// Now the session has 2 messages
	retrieved, err = store.GetSession(t.Context(), "lazy-session")
	require.NoError(t, err)
	assert.Len(t, retrieved.Messages, 2)
}

func TestStorePermissions(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "test_permissions.db")

	store, err := NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	// Create a session with permissions
	session := &Session{
		ID:        "permissions-session",
		CreatedAt: time.Now(),
		Permissions: &PermissionsConfig{
			Allow: []string{"read_*", "think"},
			Deny:  []string{"shell:cmd=rm*", "dangerous_tool"},
		},
	}

	// Store the session
	err = store.AddSession(t.Context(), session)
	require.NoError(t, err)

	// Retrieve the session
	retrieved, err := store.GetSession(t.Context(), "permissions-session")
	require.NoError(t, err)
	require.NotNil(t, retrieved.Permissions)

	assert.Equal(t, []string{"read_*", "think"}, retrieved.Permissions.Allow)
	assert.Equal(t, []string{"shell:cmd=rm*", "dangerous_tool"}, retrieved.Permissions.Deny)
}

func TestStorePermissions_NilPermissions(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "test_nil_permissions.db")

	store, err := NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	// Create a session without permissions (legacy behavior)
	session := &Session{
		ID:        "no-permissions-session",
		CreatedAt: time.Now(),
	}

	// Store the session
	err = store.AddSession(t.Context(), session)
	require.NoError(t, err)

	// Retrieve the session
	retrieved, err := store.GetSession(t.Context(), "no-permissions-session")
	require.NoError(t, err)

	// Permissions should be nil
	assert.Nil(t, retrieved.Permissions)
}

func TestUpdateSession_Permissions(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "test_update_permissions.db")

	store, err := NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	// Create a session without permissions
	session := &Session{
		ID:        "update-permissions-session",
		CreatedAt: time.Now(),
	}

	err = store.AddSession(t.Context(), session)
	require.NoError(t, err)

	// Update with permissions
	session.Permissions = &PermissionsConfig{
		Allow: []string{"safe_*"},
		Deny:  []string{"dangerous_*"},
	}

	err = store.UpdateSession(t.Context(), session)
	require.NoError(t, err)

	// Retrieve and verify
	retrieved, err := store.GetSession(t.Context(), "update-permissions-session")
	require.NoError(t, err)
	require.NotNil(t, retrieved.Permissions)

	assert.Equal(t, []string{"safe_*"}, retrieved.Permissions.Allow)
	assert.Equal(t, []string{"dangerous_*"}, retrieved.Permissions.Deny)
}

func TestAgentModelOverrides_SQLite(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "test_model_overrides.db")

	store, err := NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	// Create a session with model overrides
	session := &Session{
		ID:        "model-override-session",
		Title:     "Test Session",
		CreatedAt: time.Now(),
		AgentModelOverrides: map[string]string{
			"root":       "openai/gpt-4o",
			"researcher": "anthropic/claude-sonnet-4-0",
		},
	}

	// Store the session
	err = store.AddSession(t.Context(), session)
	require.NoError(t, err)

	// Retrieve the session
	retrieved, err := store.GetSession(t.Context(), "model-override-session")
	require.NoError(t, err)
	require.NotNil(t, retrieved)

	// Verify model overrides were persisted
	assert.Len(t, retrieved.AgentModelOverrides, 2)
	assert.Equal(t, "openai/gpt-4o", retrieved.AgentModelOverrides["root"])
	assert.Equal(t, "anthropic/claude-sonnet-4-0", retrieved.AgentModelOverrides["researcher"])
}

func TestAgentModelOverrides_Update(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "test_model_overrides_update.db")

	store, err := NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	// Create a session without model overrides
	session := &Session{
		ID:        "update-model-override-session",
		Title:     "Test Session",
		CreatedAt: time.Now(),
	}

	err = store.AddSession(t.Context(), session)
	require.NoError(t, err)

	// Update the session with model overrides
	session.AgentModelOverrides = map[string]string{
		"root": "google/gemini-2.5-flash",
	}

	err = store.UpdateSession(t.Context(), session)
	require.NoError(t, err)

	// Retrieve and verify
	retrieved, err := store.GetSession(t.Context(), "update-model-override-session")
	require.NoError(t, err)
	require.NotNil(t, retrieved)

	assert.Len(t, retrieved.AgentModelOverrides, 1)
	assert.Equal(t, "google/gemini-2.5-flash", retrieved.AgentModelOverrides["root"])
}

func TestAgentModelOverrides_EmptyMap(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "test_model_overrides_empty.db")

	store, err := NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	// Create a session without model overrides (nil map)
	session := &Session{
		ID:        "no-override-session",
		Title:     "Test Session",
		CreatedAt: time.Now(),
	}

	err = store.AddSession(t.Context(), session)
	require.NoError(t, err)

	// Retrieve the session
	retrieved, err := store.GetSession(t.Context(), "no-override-session")
	require.NoError(t, err)
	require.NotNil(t, retrieved)

	// Verify no model overrides (should be nil or empty)
	assert.Empty(t, retrieved.AgentModelOverrides)
}

func TestNewSQLiteSessionStore_RejectsNewerDatabase(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_newer_db.db")

	// Create a valid store first (applies all known migrations)
	store, err := NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	// Inject a future migration into the database to simulate a newer version
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(),
		"INSERT INTO migrations (id, name, description, applied_at) VALUES (?, ?, ?, ?)",
		9999, "9999_future_migration", "Added by a newer version", "2099-01-01T00:00:00Z")
	require.NoError(t, err)
	db.Close()

	// Opening the store should fail with a clear error about version mismatch
	_, err = NewSQLiteSessionStore(dbPath)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNewerDatabase)
	assert.Contains(t, err.Error(), "9999")
	assert.Contains(t, err.Error(), "upgrade docker-agent")
}

func TestNewSQLiteSessionStore_MigrationFailureRecovery(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_migration_recovery.db")
	backupPath := dbPath + ".bak"

	// Create a corrupted database file that will fail migrations
	err := os.WriteFile(dbPath, []byte("not a valid sqlite database"), 0o644)
	require.NoError(t, err)

	// Opening should trigger recovery: backup the corrupt file and create fresh db
	store, err := NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	// Verify a backup was created
	_, err = os.Stat(backupPath)
	require.NoError(t, err, "backup file should exist")

	// Verify the store works with the fresh database
	session := &Session{
		ID:        "test-session",
		CreatedAt: time.Now(),
	}
	err = store.AddSession(t.Context(), session)
	require.NoError(t, err)

	retrieved, err := store.GetSession(t.Context(), "test-session")
	require.NoError(t, err)
	assert.Equal(t, "test-session", retrieved.ID)
}

func TestBackupDatabase(t *testing.T) {
	t.Run("backs up existing database file", func(t *testing.T) {
		tempDir := t.TempDir()
		dbPath := filepath.Join(tempDir, "test.db")
		backupPath := dbPath + ".bak"

		// Create a file to backup
		err := os.WriteFile(dbPath, []byte("test content"), 0o644)
		require.NoError(t, err)

		// Also create WAL and SHM files
		err = os.WriteFile(dbPath+"-wal", []byte("wal content"), 0o644)
		require.NoError(t, err)
		err = os.WriteFile(dbPath+"-shm", []byte("shm content"), 0o644)
		require.NoError(t, err)

		// Backup the database
		err = backupDatabase(dbPath)
		require.NoError(t, err)

		// Original should be gone
		_, err = os.Stat(dbPath)
		assert.True(t, os.IsNotExist(err), "original file should be moved")

		// WAL and SHM should also be gone
		_, err = os.Stat(dbPath + "-wal")
		assert.True(t, os.IsNotExist(err), "WAL file should be moved")
		_, err = os.Stat(dbPath + "-shm")
		assert.True(t, os.IsNotExist(err), "SHM file should be moved")

		// Check backup files exist
		_, err = os.Stat(backupPath)
		require.NoError(t, err, "main backup should exist")
		_, err = os.Stat(backupPath + "-wal")
		require.NoError(t, err, "WAL backup should exist")
		_, err = os.Stat(backupPath + "-shm")
		require.NoError(t, err, "SHM backup should exist")
	})

	t.Run("handles nonexistent file gracefully", func(t *testing.T) {
		tempDir := t.TempDir()
		dbPath := filepath.Join(tempDir, "nonexistent.db")

		// Backup should succeed (nothing to backup)
		err := backupDatabase(dbPath)
		require.NoError(t, err)
	})
}

// TestOrphanedSubsessionReference verifies that loading sessions gracefully
// handles orphaned subsession references (where the subsession was deleted
// but the reference in session_items remains).
func TestOrphanedSubsessionReference(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "test_orphaned.db")

	store, err := NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	sqliteStore := store.(*SQLiteSessionStore)

	// Create parent session
	parentSession := &Session{
		ID:        "parent-session",
		CreatedAt: time.Now(),
	}
	err = store.AddSession(t.Context(), parentSession)
	require.NoError(t, err)

	// Add a message to parent
	_, err = store.AddMessage(t.Context(), "parent-session", UserMessage("Start task"))
	require.NoError(t, err)

	// Create and add a sub-session
	subSession := &Session{
		ID:        "sub-session-to-delete",
		CreatedAt: time.Now(),
		Messages: []Item{
			NewMessageItem(UserMessage("Sub task")),
		},
	}
	err = store.AddSubSession(t.Context(), "parent-session", subSession)
	require.NoError(t, err)

	// Verify session loads correctly before deletion
	retrieved, err := store.GetSession(t.Context(), "parent-session")
	require.NoError(t, err)
	assert.Len(t, retrieved.Messages, 2) // user message + subsession

	// Now delete the sub-session directly from the database
	// This simulates a scenario where the sub-session is deleted but the
	// reference in session_items remains (the FK sets it to NULL)
	_, err = sqliteStore.db.ExecContext(t.Context(), "DELETE FROM sessions WHERE id = ?", "sub-session-to-delete")
	require.NoError(t, err)

	// Loading the parent session should gracefully skip the orphaned reference
	retrieved, err = store.GetSession(t.Context(), "parent-session")
	require.NoError(t, err)
	assert.Len(t, retrieved.Messages, 1) // Only the user message remains
	assert.Equal(t, "Start task", retrieved.Messages[0].Message.Message.Content)

	// GetSessions should also work without error
	sessions, err := store.GetSessions(t.Context())
	require.NoError(t, err)
	assert.Len(t, sessions, 1)
	assert.Equal(t, "parent-session", sessions[0].ID)
}

// TestMigration_ExistingMessagesToSessionItems verifies that the migration
// properly converts legacy messages JSON to session_items table.
func TestMigration_ExistingMessagesToSessionItems(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "test_migration.db")

	// First, create a database with the legacy schema (before migrations)
	db, err := sql.Open("sqlite", tempDB)
	require.NoError(t, err)

	// Create minimal schema
	_, err = db.ExecContext(t.Context(), `
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			messages TEXT,
			created_at TEXT
		)
	`)
	require.NoError(t, err)

	// Insert a legacy session with messages
	legacyMessages := []Item{
		NewMessageItem(UserMessage("Legacy message 1")),
		NewMessageItem(&Message{
			AgentName: "legacy-agent",
			Message: chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "Legacy response",
			},
		}),
	}
	legacyJSON, err := json.Marshal(legacyMessages)
	require.NoError(t, err)

	_, err = db.ExecContext(t.Context(),
		"INSERT INTO sessions (id, messages, created_at) VALUES (?, ?, ?)",
		"migration-test-session", string(legacyJSON), time.Now().Format(time.RFC3339))
	require.NoError(t, err)

	db.Close()

	// Now open with the store, which runs migrations
	store, err := NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	// Session should be readable via the new API
	retrieved, err := store.GetSession(t.Context(), "migration-test-session")
	require.NoError(t, err)

	assert.Len(t, retrieved.Messages, 2)
	assert.Equal(t, "Legacy message 1", retrieved.Messages[0].Message.Message.Content)
	assert.Equal(t, "legacy-agent", retrieved.Messages[1].Message.AgentName)
}

func TestParseRelativeSessionRef(t *testing.T) {
	tests := []struct {
		name       string
		ref        string
		wantOffset int
		wantIsRel  bool
	}{
		{"last session", "-1", 1, true},
		{"second to last", "-2", 2, true},
		{"tenth session", "-10", 10, true},
		{"regular ID", "abc123", 0, false},
		{"UUID-like", "550e8400-e29b-41d4-a716-446655440000", 0, false},
		{"positive number", "1", 0, false},
		{"zero", "0", 0, false},
		{"negative zero", "-0", 0, false},
		{"not a number", "-abc", 0, false},
		{"empty string", "", 0, false},
		{"just dash", "-", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			offset, isRel := parseRelativeSessionRef(tt.ref)
			assert.Equal(t, tt.wantOffset, offset)
			assert.Equal(t, tt.wantIsRel, isRel)
		})
	}
}

func TestResolveSessionID_SQLite(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "test_resolve.db")

	store, err := NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	// Create sessions with known timestamps
	baseTime := time.Now()
	sessions := []struct {
		id        string
		createdAt time.Time
	}{
		{"oldest", baseTime.Add(-3 * time.Hour)},
		{"middle", baseTime.Add(-2 * time.Hour)},
		{"newest", baseTime.Add(-1 * time.Hour)},
	}

	for _, s := range sessions {
		err := store.AddSession(t.Context(), &Session{
			ID:        s.id,
			CreatedAt: s.createdAt,
		})
		require.NoError(t, err)
	}

	t.Run("resolves -1 to newest session", func(t *testing.T) {
		id, err := ResolveSessionID(t.Context(), store, "-1")
		require.NoError(t, err)
		assert.Equal(t, "newest", id)
	})

	t.Run("resolves -2 to middle session", func(t *testing.T) {
		id, err := ResolveSessionID(t.Context(), store, "-2")
		require.NoError(t, err)
		assert.Equal(t, "middle", id)
	})

	t.Run("resolves -3 to oldest session", func(t *testing.T) {
		id, err := ResolveSessionID(t.Context(), store, "-3")
		require.NoError(t, err)
		assert.Equal(t, "oldest", id)
	})

	t.Run("returns error for out of range offset", func(t *testing.T) {
		_, err := ResolveSessionID(t.Context(), store, "-4")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "out of range")
	})

	t.Run("returns non-relative ID unchanged", func(t *testing.T) {
		id, err := ResolveSessionID(t.Context(), store, "some-uuid")
		require.NoError(t, err)
		assert.Equal(t, "some-uuid", id)
	})
}

func TestResolveSessionID_ExcludesSubSessions(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "test_resolve_subsessions.db")

	store, err := NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	sqliteStore := store.(*SQLiteSessionStore)

	// Create a parent session
	parent := &Session{
		ID:        "parent",
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	err = store.AddSession(t.Context(), parent)
	require.NoError(t, err)

	// Create a sub-session directly in the database to avoid the AddSubSession complexity
	// This simulates an existing sub-session without going through the full API
	subSessionTime := time.Now().Add(-1 * time.Hour)
	_, err = sqliteStore.db.ExecContext(t.Context(),
		`INSERT INTO sessions (id, tools_approved, input_tokens, output_tokens, title, cost, send_user_message, max_iterations, working_dir, created_at, starred, permissions, agent_model_overrides, custom_models_used, thinking, parent_id)
		 VALUES (?, 0, 0, 0, '', 0, 1, 0, '', ?, 0, '', '{}', '[]', 1, ?)`,
		"subsession", subSessionTime.Format(time.RFC3339), "parent")
	require.NoError(t, err)

	// Create another root session (most recent root)
	root2 := &Session{
		ID:        "root2",
		CreatedAt: time.Now(),
	}
	err = store.AddSession(t.Context(), root2)
	require.NoError(t, err)

	// -1 should resolve to root2, not the subsession
	id, err := ResolveSessionID(t.Context(), store, "-1")
	require.NoError(t, err)
	assert.Equal(t, "root2", id)

	// -2 should resolve to parent
	id, err = ResolveSessionID(t.Context(), store, "-2")
	require.NoError(t, err)
	assert.Equal(t, "parent", id)
}

func TestResolveSessionID_InMemory(t *testing.T) {
	store := NewInMemorySessionStore()

	// Create sessions with known timestamps
	baseTime := time.Now()
	sessions := []struct {
		id        string
		createdAt time.Time
	}{
		{"oldest", baseTime.Add(-3 * time.Hour)},
		{"middle", baseTime.Add(-2 * time.Hour)},
		{"newest", baseTime.Add(-1 * time.Hour)},
	}

	for _, s := range sessions {
		err := store.AddSession(t.Context(), &Session{
			ID:        s.id,
			CreatedAt: s.createdAt,
		})
		require.NoError(t, err)
	}

	t.Run("resolves -1 to newest session", func(t *testing.T) {
		id, err := ResolveSessionID(t.Context(), store, "-1")
		require.NoError(t, err)
		assert.Equal(t, "newest", id)
	})

	t.Run("resolves -2 to middle session", func(t *testing.T) {
		id, err := ResolveSessionID(t.Context(), store, "-2")
		require.NoError(t, err)
		assert.Equal(t, "middle", id)
	})

	t.Run("returns error for out of range offset", func(t *testing.T) {
		_, err := ResolveSessionID(t.Context(), store, "-4")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "out of range")
	})

	t.Run("returns non-relative ID unchanged", func(t *testing.T) {
		id, err := ResolveSessionID(t.Context(), store, "some-uuid")
		require.NoError(t, err)
		assert.Equal(t, "some-uuid", id)
	})
}
