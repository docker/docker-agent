package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/concurrent"
)

var (
	ErrEmptyID       = errors.New("session ID cannot be empty")
	ErrNotFound      = errors.New("session not found")
	ErrNewerDatabase = errors.New("session database was created by a newer version of docker-agent")
)

// IsRelativeSessionRef reports whether ref is a relative session reference
// (e.g., "-1", "-2"). Explicit IDs (anything else, including UUIDs) return
// false. Callers use this to distinguish a user-supplied concrete ID — which
// may legitimately not exist yet — from a relative offset that must resolve
// against existing sessions.
func IsRelativeSessionRef(ref string) bool {
	_, isRelative := parseRelativeSessionRef(ref)
	return isRelative
}

// parseRelativeSessionRef checks if ref is a relative session reference (e.g., "-1", "-2")
// and returns the offset and whether it's a relative reference.
// Returns (1, true) for "-1", (2, true) for "-2", etc.
// Returns (0, false) if not a relative reference.
func parseRelativeSessionRef(ref string) (offset int, isRelative bool) {
	if !strings.HasPrefix(ref, "-") {
		return 0, false
	}

	// Try to parse as negative integer
	n, err := strconv.Atoi(ref)
	if err != nil || n >= 0 {
		return 0, false
	}

	return -n, true
}

// ResolveSessionID resolves a session reference to an actual session ID.
// Supports relative references like "-1" (last session), "-2" (second to last), etc.
// If the reference is not relative, it returns the input unchanged.
func ResolveSessionID(ctx context.Context, store Store, ref string) (string, error) {
	offset, isRelative := parseRelativeSessionRef(ref)
	if !isRelative {
		return ref, nil
	}

	summaries, err := store.GetSessionSummaries(ctx)
	if err != nil {
		return "", fmt.Errorf("getting session summaries: %w", err)
	}

	index := offset - 1
	if index >= len(summaries) {
		return "", fmt.Errorf("session offset %d out of range (have %d sessions)", offset, len(summaries))
	}

	return summaries[index].ID, nil
}

// Summary contains lightweight session metadata for listing purposes.
// This is used instead of loading full Session objects with all messages.
type Summary struct {
	ID          string
	Title       string
	CreatedAt   time.Time
	Starred     bool
	NumMessages int
	Cost        float64
	WorkingDir  string
	Attributes  map[string]string
}

// Store defines the interface for session storage
type Store interface {
	// === Core session operations ===
	AddSession(ctx context.Context, session *Session) error
	GetSession(ctx context.Context, id string) (*Session, error)
	GetSessions(ctx context.Context) ([]*Session, error)
	GetSessionSummaries(ctx context.Context) ([]Summary, error)
	DeleteSession(ctx context.Context, id string) error
	UpdateSession(ctx context.Context, session *Session) error // Updates metadata only (not messages/items)
	SetSessionStarred(ctx context.Context, id string, starred bool) error

	// === Granular item operations ===

	// AddMessage adds a message to a session at the next position.
	// Returns the ID of the created message item.
	AddMessage(ctx context.Context, sessionID string, msg *Message) (int64, error)

	// UpdateMessage updates an existing message by its ID.
	// This is called on each streaming delta to keep the persisted message
	// in sync with the in-progress content, and once more with the final
	// payload when the message completes.
	UpdateMessage(ctx context.Context, messageID int64, msg *Message) error

	// AddSubSession creates a sub-session and links it to the parent.
	// The sub-session is stored as a separate session row with parent_id set.
	AddSubSession(ctx context.Context, parentSessionID string, subSession *Session) error

	// AddSummary adds a summary item to a session at the next position.
	// item.FirstKeptEntry is the index of the first message kept verbatim during
	// compaction; item.Cost/Model/Usage attribute the summary's spend (zero
	// values when nothing was billed).
	AddSummary(ctx context.Context, sessionID string, item Item) error

	// AddError appends a recorded error item to a session at the next position.
	// Persisting failures lets them survive a reload and travel with a JSON export.
	AddError(ctx context.Context, sessionID string, e *Error) error

	// === Granular metadata updates ===

	// UpdateSessionTokens updates only token/cost fields
	UpdateSessionTokens(ctx context.Context, sessionID string, inputTokens, outputTokens int64, cost float64) error

	// UpdateSessionTitle updates only the title
	UpdateSessionTitle(ctx context.Context, sessionID, title string) error

	// Close releases any resources held by the store (e.g., database connections).
	Close() error
}

type InMemorySessionStore struct {
	sessions  *concurrent.Map[string, *Session]
	messageID atomic.Int64 // counter for message IDs, incremented via Add(1)
}

func NewInMemorySessionStore() Store {
	return &InMemorySessionStore{
		sessions: concurrent.NewMap[string, *Session](),
	}
}

func (s *InMemorySessionStore) AddSession(_ context.Context, session *Session) error {
	if session.ID == "" {
		return ErrEmptyID
	}
	s.sessions.Store(session.ID, session)
	return nil
}

func (s *InMemorySessionStore) GetSession(_ context.Context, id string) (*Session, error) {
	if id == "" {
		return nil, ErrEmptyID
	}
	session, exists := s.sessions.Load(id)
	if !exists {
		return nil, ErrNotFound
	}
	return session, nil
}

func (s *InMemorySessionStore) GetSessions(_ context.Context) ([]*Session, error) {
	sessions := make([]*Session, 0, s.sessions.Length())
	s.sessions.Range(func(key string, value *Session) bool {
		sessions = append(sessions, value)
		return true
	})
	return sessions, nil
}

func (s *InMemorySessionStore) GetSessionSummaries(_ context.Context) ([]Summary, error) {
	summaries := make([]Summary, 0, s.sessions.Length())
	s.sessions.Range(func(_ string, value *Session) bool {
		if value.ParentID != "" {
			return true
		}
		_, _, cost := value.TokensAndCost()
		summaries = append(summaries, Summary{
			ID:          value.ID,
			Title:       value.TitleSnapshot(),
			CreatedAt:   value.CreatedAt,
			Starred:     value.Starred,
			NumMessages: value.MessageCount(),
			Cost:        cost,
			WorkingDir:  value.WorkingDir,
			Attributes:  value.AttributesSnapshot(),
		})
		return true
	})
	slices.SortFunc(summaries, func(a, b Summary) int {
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	return summaries, nil
}

func (s *InMemorySessionStore) DeleteSession(_ context.Context, id string) error {
	if id == "" {
		return ErrEmptyID
	}
	_, exists := s.sessions.Load(id)
	if !exists {
		return ErrNotFound
	}
	s.sessions.Delete(id)
	return nil
}

// UpdateSession updates an existing session, or creates it if it doesn't exist (upsert).
// This enables lazy session persistence - sessions are only stored when they have content.
// Note: Like SQLite, this only stores metadata. Messages are stored separately via AddMessage.
func (s *InMemorySessionStore) UpdateSession(_ context.Context, session *Session) error {
	if session.ID == "" {
		return ErrEmptyID
	}

	// Snapshot the input session under its mu so the field copy
	// doesn't race with concurrent writers (e.g. the runtime stream
	// goroutine updating token counts via SetUsage).
	// MAINTENANCE: when adding new persisted fields to Session, add them here too.
	session.mu.RLock()
	newSession := &Session{
		ID:                  session.ID,
		Title:               session.Title,
		Evals:               session.Evals,
		CreatedAt:           session.CreatedAt,
		ToolsApproved:       session.ToolsApproved,
		SafetyPolicy:        session.SafetyPolicy,
		HideToolResults:     session.HideToolResults,
		WorkingDir:          session.WorkingDir,
		SendUserMessage:     session.SendUserMessage,
		MaxIterations:       session.MaxIterations,
		Starred:             session.Starred,
		InputTokens:         session.InputTokens,
		OutputTokens:        session.OutputTokens,
		Cost:                session.Cost,
		Permissions:         session.Permissions.Clone(),
		Attributes:          maps.Clone(session.Attributes),
		AgentModelOverrides: cloneStringMap(session.AgentModelOverrides),
		CustomModelsUsed:    cloneStringSlice(session.CustomModelsUsed),
		InstructionContext:  cloneInstructionContext(session.InstructionContext),
		AttachedFiles:       slices.Clone(session.AttachedFiles),
		ParentID:            session.ParentID,
	}
	session.mu.RUnlock()

	// Preserve existing messages if session already exists
	if existing, exists := s.sessions.Load(session.ID); exists {
		existing.mu.RLock()
		newSession.Messages = make([]Item, len(existing.Messages))
		copy(newSession.Messages, existing.Messages)
		existing.mu.RUnlock()
	}

	s.sessions.Store(session.ID, newSession)
	return nil
}

// SetSessionStarred sets the starred status of a session.
func (s *InMemorySessionStore) SetSessionStarred(_ context.Context, id string, starred bool) error {
	if id == "" {
		return ErrEmptyID
	}
	session, exists := s.sessions.Load(id)
	if !exists {
		return ErrNotFound
	}
	session.Starred = starred
	s.sessions.Store(id, session)
	return nil
}

// AddMessage adds a message to a session at the next position.
// Returns the ID of the created message (for in-memory, this is a simple counter).
func (s *InMemorySessionStore) AddMessage(_ context.Context, sessionID string, msg *Message) (int64, error) {
	if sessionID == "" {
		return 0, ErrEmptyID
	}
	session, exists := s.sessions.Load(sessionID)
	if !exists {
		return 0, ErrNotFound
	}
	// Deep-copy before mutating ID. The caller's pointer may be held
	// concurrently by another goroutine (snapshotItems → cloneMessage),
	// and writing msg.ID directly races with those reads.
	stored := cloneMessage(msg)
	id := s.messageID.Add(1)
	stored.ID = id
	session.AddMessage(stored)
	return id, nil
}

// UpdateMessage updates an existing message by its ID.
func (s *InMemorySessionStore) UpdateMessage(_ context.Context, messageID int64, msg *Message) error {
	// Create a deep copy of the message to avoid mutating the caller's pointer,
	// which may be shared with another Session object.
	updated := cloneMessage(msg)
	updated.ID = messageID

	// For in-memory store, we need to find the message across all sessions
	var found bool
	s.sessions.Range(func(_ string, session *Session) bool {
		session.mu.Lock()
		defer session.mu.Unlock()
		for i := range session.Messages {
			if session.Messages[i].Message == nil || session.Messages[i].Message.ID != messageID {
				continue
			}
			session.Messages[i].Message = updated
			found = true
			return false
		}
		return true
	})
	if !found {
		return ErrNotFound
	}
	return nil
}

// AddSubSession creates a sub-session and links it to the parent.
func (s *InMemorySessionStore) AddSubSession(_ context.Context, parentSessionID string, subSession *Session) error {
	if parentSessionID == "" {
		return ErrEmptyID
	}
	parent, exists := s.sessions.Load(parentSessionID)
	if !exists {
		return ErrNotFound
	}
	subSession.ParentID = parentSessionID
	s.sessions.Store(subSession.ID, subSession)
	parent.AddSubSession(subSession)
	return nil
}

// AddSummary adds a summary item to a session at the next position.
func (s *InMemorySessionStore) AddSummary(_ context.Context, sessionID string, item Item) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	session, exists := s.sessions.Load(sessionID)
	if !exists {
		return ErrNotFound
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	session.Messages = append(session.Messages, item)
	return nil
}

// AddError appends a recorded error item to a session at the next position.
func (s *InMemorySessionStore) AddError(_ context.Context, sessionID string, e *Error) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	session, exists := s.sessions.Load(sessionID)
	if !exists {
		return ErrNotFound
	}
	errCopy := *e
	session.AddError(&errCopy)
	return nil
}

// querier is an interface that abstracts *sql.DB and *sql.Tx for query operations.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// SQLiteSessionStore implements Store using SQLite
type SQLiteSessionStore struct {
	db *sql.DB
}

// sessionSelectColumns is the canonical SELECT list for the sessions table.
// The column order matches what scanSession expects; all read paths use this
// constant so that adding a column requires updating exactly one place.
const sessionSelectColumns = `id, tools_approved, safety_policy, input_tokens, output_tokens, title, cost, send_user_message, max_iterations, working_dir, created_at, starred, permissions, agent_model_overrides, custom_models_used, thinking, parent_id, instruction_context, attributes`

// sessionPersistedFields holds the encoded form of a Session's JSON-bearing
// columns plus the SQL representation of parent_id (nil for the empty
// string, which keeps the foreign key constraint happy).
type sessionPersistedFields struct {
	PermissionsJSON         string
	AgentModelOverridesJSON string
	CustomModelsUsedJSON    string
	InstructionContextJSON  string
	AttributesJSON          string
	ParentID                any // string or nil
}

// sessionPersistedFieldsOf marshals the JSON-bearing columns of session and
// derives the SQL parent_id value. INSERT/UPDATE call sites use this helper
// so the marshaling rules ("" / "{}" / "[]" defaults, NULL parent_id) live
// in one place.
func sessionPersistedFieldsOf(session *Session) (sessionPersistedFields, error) {
	var f sessionPersistedFields

	attributes := session.AttributesSnapshot()
	f.AttributesJSON = "{}"
	if len(attributes) > 0 {
		attributesBytes, err := json.Marshal(attributes)
		if err != nil {
			return f, err
		}
		f.AttributesJSON = string(attributesBytes)
	}

	if session.Permissions != nil {
		permBytes, err := json.Marshal(session.Permissions)
		if err != nil {
			return f, err
		}
		f.PermissionsJSON = string(permBytes)
	}

	f.AgentModelOverridesJSON = "{}"
	if len(session.AgentModelOverrides) > 0 {
		overridesBytes, err := json.Marshal(session.AgentModelOverrides)
		if err != nil {
			return f, err
		}
		f.AgentModelOverridesJSON = string(overridesBytes)
	}

	f.CustomModelsUsedJSON = "[]"
	if len(session.CustomModelsUsed) > 0 {
		customBytes, err := json.Marshal(session.CustomModelsUsed)
		if err != nil {
			return f, err
		}
		f.CustomModelsUsedJSON = string(customBytes)
	}

	if session.InstructionContext != nil {
		contextBytes, err := json.Marshal(session.InstructionContext)
		if err != nil {
			return f, err
		}
		f.InstructionContextJSON = string(contextBytes)
	}

	// Use NULL for empty parent_id to avoid foreign key constraint issues.
	if session.ParentID != "" {
		f.ParentID = session.ParentID
	}

	return f, nil
}

// UpdateSessionTokens updates only token/cost fields.
func (s *InMemorySessionStore) UpdateSessionTokens(_ context.Context, sessionID string, inputTokens, outputTokens int64, cost float64) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	session, exists := s.sessions.Load(sessionID)
	if !exists {
		return ErrNotFound
	}
	session.SetTokensAndCost(inputTokens, outputTokens, cost)
	return nil
}

// UpdateSessionTitle updates only the title.
func (s *InMemorySessionStore) UpdateSessionTitle(_ context.Context, sessionID, title string) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	session, exists := s.sessions.Load(sessionID)
	if !exists {
		return ErrNotFound
	}
	session.SetTitle(title)
	return nil
}

// Close is a no-op for in-memory stores.
func (s *InMemorySessionStore) Close() error {
	return nil
}

// NewSQLiteSessionStoreFromDB wraps an already-open *sql.DB in a session store,
// running the bootstrap schema and migrations against it. The caller retains
// ownership of db: it is not closed on error, and Store.Close() will close it
// when the store is closed.
//
// This is intended primarily for tests that want to use an in-memory database
// (sql.Open("sqlite", ":memory:")) or pre-seed a database with non-default
// state. Production callers should use sqlitestore.New.
func NewSQLiteSessionStoreFromDB(ctx context.Context, db *sql.DB) (*SQLiteSessionStore, error) {
	if db == nil {
		return nil, errors.New("db is nil")
	}
	if err := setupAndMigrate(ctx, db); err != nil {
		return nil, err
	}
	return &SQLiteSessionStore{db: db}, nil
}

// setupAndMigrate creates the bootstrap sessions table (if missing) and runs
// all pending schema migrations. The bootstrap schema only declares the
// columns the very first migration expects to find; later columns are added
// by subsequent ALTER TABLE migrations.
func setupAndMigrate(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			messages TEXT,
			created_at TEXT
		)
	`)
	if err != nil {
		return err
	}

	migrationManager := NewMigrationManager(db)
	return migrationManager.InitializeMigrations(ctx)
}

// parseCreatedAt parses a created_at column value. A corrupt timestamp in a
// single row must not make every session unloadable, so parse failures are
// logged and the zero time is returned instead of an error.
func parseCreatedAt(createdAtStr string) time.Time {
	t, err := time.Parse(time.RFC3339, createdAtStr)
	if err != nil {
		slog.Warn("Invalid created_at in session database, using zero time", "value", createdAtStr, "error", err)
		return time.Time{}
	}
	return t
}

// AddSession adds a new session to the store, including any messages
func (s *SQLiteSessionStore) AddSession(ctx context.Context, session *Session) error {
	if session.ID == "" {
		return ErrEmptyID
	}

	fields, err := sessionPersistedFieldsOf(session)
	if err != nil {
		return err
	}

	// Use a transaction to insert session and its items
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO sessions (
			id, tools_approved, safety_policy, input_tokens, output_tokens, title, cost, send_user_message,
			max_iterations, working_dir, created_at, permissions, agent_model_overrides,
			custom_models_used, thinking, parent_id, instruction_context, attributes
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.ID, session.ToolsApproved, string(session.SafetyPolicy), session.InputTokens, session.OutputTokens, session.Title,
		session.Cost, session.SendUserMessage, session.MaxIterations, session.WorkingDir,
		session.CreatedAt.Format(time.RFC3339), fields.PermissionsJSON, fields.AgentModelOverridesJSON,
		fields.CustomModelsUsedJSON, false, fields.ParentID, fields.InstructionContextJSON, fields.AttributesJSON)
	if err != nil {
		return err
	}

	// Insert all messages into session_items
	for position, item := range session.Messages {
		if err := s.addItemTx(ctx, tx, session.ID, position, item); err != nil {
			return fmt.Errorf("adding item at position %d: %w", position, err)
		}
	}

	return tx.Commit()
}

// scanSession scans a single row into a Session struct.
// Note: Messages are loaded separately from session_items table.
// The thinking column is read but discarded — it is kept in the schema for
// backward compatibility with older docker-agent versions that wrote it.
func scanSession(scanner interface {
	Scan(dest ...any) error
},
) (*Session, error) {
	var (
		sess                    Session
		safetyPolicy            sql.NullString
		workingDir              sql.NullString
		permissionsJSON         sql.NullString
		parentID                sql.NullString
		agentModelOverridesJSON string
		customModelsUsedJSON    string
		instructionContextJSON  sql.NullString
		attributesJSON          sql.NullString
		createdAtStr            string
		thinking                bool // discarded
	)

	err := scanner.Scan(
		&sess.ID, &sess.ToolsApproved, &safetyPolicy, &sess.InputTokens, &sess.OutputTokens,
		&sess.Title, &sess.Cost, &sess.SendUserMessage, &sess.MaxIterations,
		&workingDir, &createdAtStr, &sess.Starred, &permissionsJSON,
		&agentModelOverridesJSON, &customModelsUsedJSON, &thinking, &parentID, &instructionContextJSON, &attributesJSON,
	)
	if err != nil {
		return nil, err
	}

	sess.CreatedAt = parseCreatedAt(createdAtStr)
	sess.SafetyPolicy = SafetyPolicy(safetyPolicy.String)
	sess.WorkingDir = workingDir.String
	sess.ParentID = parentID.String

	if permissionsJSON.Valid && permissionsJSON.String != "" {
		sess.Permissions = &PermissionsConfig{}
		if err := json.Unmarshal([]byte(permissionsJSON.String), sess.Permissions); err != nil {
			return nil, err
		}
	}

	if agentModelOverridesJSON != "" && agentModelOverridesJSON != "{}" {
		if err := json.Unmarshal([]byte(agentModelOverridesJSON), &sess.AgentModelOverrides); err != nil {
			return nil, err
		}
	}

	if customModelsUsedJSON != "" && customModelsUsedJSON != "[]" {
		if err := json.Unmarshal([]byte(customModelsUsedJSON), &sess.CustomModelsUsed); err != nil {
			return nil, err
		}
	}

	if instructionContextJSON.Valid && instructionContextJSON.String != "" {
		sess.InstructionContext = &InstructionContextState{}
		if err := json.Unmarshal([]byte(instructionContextJSON.String), sess.InstructionContext); err != nil {
			return nil, err
		}
	}

	sess.Attributes, err = decodeAttributes(attributesJSON)
	if err != nil {
		return nil, fmt.Errorf("unmarshaling session attributes: %w", err)
	}

	return &sess, nil
}

func decodeAttributes(value sql.NullString) (map[string]string, error) {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil, nil
	}
	var attributes map[string]string
	if err := json.Unmarshal([]byte(value.String), &attributes); err != nil {
		return nil, err
	}
	if len(attributes) == 0 {
		return nil, nil
	}
	return attributes, nil
}

// GetSession retrieves a session by ID
func (s *SQLiteSessionStore) GetSession(ctx context.Context, id string) (*Session, error) {
	if id == "" {
		return nil, ErrEmptyID
	}
	return s.loadSession(ctx, s.db, id)
}

// sessionItemRow holds the raw data from a session_items row
type sessionItemRow struct {
	position       int
	itemType       string
	agentName      sql.NullString
	messageJSON    sql.NullString
	implicit       bool
	subsessionID   sql.NullString
	summaryText    sql.NullString
	firstKeptEntry int
	cost           float64
	model          string
	usageJSON      string
}

// loadSessionItems loads all items for a session from session_items.
// Used both as the public read path (q == s.db) and recursively from inside
// loadSession when resolving sub-sessions inside a transaction.
func (s *SQLiteSessionStore) loadSessionItems(ctx context.Context, q querier, sessionID string) ([]Item, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT position, item_type, agent_name, message_json, implicit, subsession_id, summary_text, COALESCE(first_kept_entry, 0), cost, COALESCE(model, ''), COALESCE(usage_json, '')
		 FROM session_items WHERE session_id = ? ORDER BY position`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// First, collect all raw row data so we can close the result set
	// before making any recursive calls (SQLite doesn't allow concurrent queries)
	var rawRows []sessionItemRow
	for rows.Next() {
		var row sessionItemRow
		if err := rows.Scan(&row.position, &row.itemType, &row.agentName, &row.messageJSON, &row.implicit, &row.subsessionID, &row.summaryText, &row.firstKeptEntry, &row.cost, &row.model, &row.usageJSON); err != nil {
			return nil, err
		}
		rawRows = append(rawRows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(rawRows) == 0 {
		return nil, nil
	}

	// Now process the collected rows, making recursive calls as needed
	var items []Item
	for _, row := range rawRows {
		switch row.itemType {
		case "message":
			var chatMsg chat.Message
			if err := json.Unmarshal([]byte(row.messageJSON.String), &chatMsg); err != nil {
				return nil, fmt.Errorf("unmarshaling message at position %d: %w", row.position, err)
			}
			items = append(items, Item{
				Message: &Message{
					AgentName: row.agentName.String,
					Message:   chatMsg,
					Implicit:  row.implicit,
				},
			})

		case "subsession":
			// Skip if subsession_id is NULL (can happen if the sub-session was deleted
			// and the foreign key set the reference to NULL)
			if !row.subsessionID.Valid || row.subsessionID.String == "" {
				slog.WarnContext(ctx, "Skipping subsession item with NULL reference", "session_id", sessionID, "position", row.position)
				continue
			}
			// Recursively load sub-session
			subSession, err := s.loadSession(ctx, q, row.subsessionID.String)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					// Sub-session was deleted but item reference remains (orphaned reference)
					slog.WarnContext(ctx, "Skipping orphaned subsession reference", "session_id", sessionID, "subsession_id", row.subsessionID.String)
					continue
				}
				return nil, fmt.Errorf("getting sub-session %s: %w", row.subsessionID.String, err)
			}
			items = append(items, Item{SubSession: subSession})

		case "summary":
			item := Item{Summary: row.summaryText.String, FirstKeptEntry: row.firstKeptEntry, Cost: row.cost, Model: row.model}
			if row.usageJSON != "" {
				var usage chat.Usage
				if err := json.Unmarshal([]byte(row.usageJSON), &usage); err != nil {
					return nil, fmt.Errorf("unmarshaling summary usage at position %d: %w", row.position, err)
				}
				item.Usage = &usage
			}
			items = append(items, item)

		case "error":
			var e Error
			if row.messageJSON.Valid && row.messageJSON.String != "" {
				if err := json.Unmarshal([]byte(row.messageJSON.String), &e); err != nil {
					return nil, fmt.Errorf("unmarshaling error at position %d: %w", row.position, err)
				}
			}
			items = append(items, Item{Error: &e})

		case "termination":
			var term Termination
			if row.messageJSON.Valid && row.messageJSON.String != "" {
				if err := json.Unmarshal([]byte(row.messageJSON.String), &term); err != nil {
					return nil, fmt.Errorf("unmarshaling termination at position %d: %w", row.position, err)
				}
			}
			items = append(items, Item{Termination: &term})
		}
	}

	return items, nil
}

// loadSession retrieves a session by ID using the supplied querier.
func (s *SQLiteSessionStore) loadSession(ctx context.Context, q querier, id string) (*Session, error) {
	row := q.QueryRowContext(ctx,
		"SELECT "+sessionSelectColumns+" FROM sessions WHERE id = ?", id)

	sess, err := scanSession(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	sess.Messages, err = s.loadSessionItems(ctx, q, id)
	if err != nil {
		return nil, fmt.Errorf("loading session items: %w", err)
	}

	return sess, nil
}

// GetSessions retrieves all root sessions (excludes sub-sessions)
func (s *SQLiteSessionStore) GetSessions(ctx context.Context) ([]*Session, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+sessionSelectColumns+" FROM sessions WHERE parent_id IS NULL OR parent_id = '' ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Collect sessions first to close the rows before loading items
	var sessions []*Session
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Load messages for each session
	for _, session := range sessions {
		items, err := s.loadSessionItems(ctx, s.db, session.ID)
		if err != nil {
			return nil, fmt.Errorf("loading items for session %s: %w", session.ID, err)
		}
		session.Messages = items
	}

	return sessions, nil
}

// GetSessionSummaries retrieves lightweight session metadata for listing (excludes sub-sessions).
// This is much faster than GetSessions as it doesn't load message content.
func (s *SQLiteSessionStore) GetSessionSummaries(ctx context.Context) ([]Summary, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, s.title, s.created_at, s.starred, s.cost, s.working_dir, s.attributes,
		        (SELECT COUNT(*) FROM session_items si WHERE si.session_id = s.id AND si.item_type = 'message')
		 FROM sessions s
		 WHERE s.parent_id IS NULL OR s.parent_id = ''
		 ORDER BY s.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []Summary
	for rows.Next() {
		var (
			summary        Summary
			createdAtStr   string
			workingDir     sql.NullString
			attributesJSON sql.NullString
		)
		if err := rows.Scan(&summary.ID, &summary.Title, &createdAtStr, &summary.Starred, &summary.Cost, &workingDir, &attributesJSON, &summary.NumMessages); err != nil {
			return nil, err
		}
		summary.CreatedAt = parseCreatedAt(createdAtStr)
		summary.WorkingDir = workingDir.String
		summary.Attributes, err = decodeAttributes(attributesJSON)
		if err != nil {
			return nil, fmt.Errorf("unmarshaling session attributes for %s: %w", summary.ID, err)
		}
		summaries = append(summaries, summary)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return summaries, nil
}

// DeleteSession deletes a session by ID
func (s *SQLiteSessionStore) DeleteSession(ctx context.Context, id string) error {
	if id == "" {
		return ErrEmptyID
	}

	result, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", id)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if rowsAffected == 0 {
		return ErrNotFound
	}

	return nil
}

// UpdateSession updates an existing session's metadata, or creates it if it doesn't exist (upsert).
// Only metadata is modified - use AddMessage, AddSubSession, AddSummary for items.
// Messages are persisted separately via events to avoid duplication.
func (s *SQLiteSessionStore) UpdateSession(ctx context.Context, session *Session) error {
	if session.ID == "" {
		return ErrEmptyID
	}

	// Snapshot the persisted fields under session.mu so the reads below
	// don't race with concurrent writers on the runtime stream goroutine
	// (SetUsage / ApplyCompaction update InputTokens/OutputTokens while a
	// stream is running). Mirrors InMemorySessionStore.UpdateSession.
	// MAINTENANCE: when adding new persisted fields to Session, add them here too.
	session.mu.RLock()
	snapshot := &Session{
		ID:                  session.ID,
		Title:               session.Title,
		CreatedAt:           session.CreatedAt,
		ToolsApproved:       session.ToolsApproved,
		SafetyPolicy:        session.SafetyPolicy,
		HideToolResults:     session.HideToolResults,
		WorkingDir:          session.WorkingDir,
		SendUserMessage:     session.SendUserMessage,
		MaxIterations:       session.MaxIterations,
		Starred:             session.Starred,
		InputTokens:         session.InputTokens,
		OutputTokens:        session.OutputTokens,
		Cost:                session.Cost,
		Permissions:         session.Permissions.Clone(),
		Attributes:          maps.Clone(session.Attributes),
		AgentModelOverrides: cloneStringMap(session.AgentModelOverrides),
		CustomModelsUsed:    cloneStringSlice(session.CustomModelsUsed),
		InstructionContext:  cloneInstructionContext(session.InstructionContext),
		ParentID:            session.ParentID,
	}
	session.mu.RUnlock()

	fields, err := sessionPersistedFieldsOf(snapshot)
	if err != nil {
		return err
	}

	// Use a transaction
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Use INSERT OR REPLACE for upsert behavior - creates if not exists, updates if exists
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sessions (
			id, tools_approved, safety_policy, input_tokens, output_tokens, title, cost, send_user_message,
			max_iterations, working_dir, created_at, starred, permissions, agent_model_overrides,
			custom_models_used, thinking, parent_id, instruction_context, attributes
		)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   title = excluded.title,
		   tools_approved = excluded.tools_approved,
		   safety_policy = excluded.safety_policy,
		   input_tokens = excluded.input_tokens,
		   output_tokens = excluded.output_tokens,
		   cost = excluded.cost,
		   send_user_message = excluded.send_user_message,
		   max_iterations = excluded.max_iterations,
		   working_dir = excluded.working_dir,
		   starred = excluded.starred,
		   permissions = excluded.permissions,
		   agent_model_overrides = excluded.agent_model_overrides,
		   custom_models_used = excluded.custom_models_used,
		   thinking = excluded.thinking,
		   parent_id = excluded.parent_id,
		   instruction_context = excluded.instruction_context,
		   attributes = excluded.attributes`,
		snapshot.ID, snapshot.ToolsApproved, string(snapshot.SafetyPolicy), snapshot.InputTokens, snapshot.OutputTokens,
		snapshot.Title, snapshot.Cost, snapshot.SendUserMessage, snapshot.MaxIterations, snapshot.WorkingDir,
		snapshot.CreatedAt.Format(time.RFC3339), snapshot.Starred, fields.PermissionsJSON, fields.AgentModelOverridesJSON,
		fields.CustomModelsUsedJSON, false, fields.ParentID, fields.InstructionContextJSON, fields.AttributesJSON)
	if err != nil {
		return err
	}

	// Note: Messages are NOT persisted here. They are persisted via events
	// (UserMessageEvent, MessageAddedEvent, etc.) to avoid duplication.

	return tx.Commit()
}

// SetSessionStarred sets the starred status of a session.
func (s *SQLiteSessionStore) SetSessionStarred(ctx context.Context, id string, starred bool) error {
	if id == "" {
		return ErrEmptyID
	}

	result, err := s.db.ExecContext(ctx, "UPDATE sessions SET starred = ? WHERE id = ?", starred, id)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if rowsAffected == 0 {
		return ErrNotFound
	}

	return nil
}

// Close closes the database connection
func (s *SQLiteSessionStore) Close() error {
	return s.db.Close()
}

// AddMessage adds a message to a session at the next position.
// Returns the ID of the created message item.
func (s *SQLiteSessionStore) AddMessage(ctx context.Context, sessionID string, msg *Message) (int64, error) {
	if sessionID == "" {
		return 0, ErrEmptyID
	}

	msgJSON, err := json.Marshal(msg.Message)
	if err != nil {
		return 0, fmt.Errorf("marshaling message: %w", err)
	}

	// Insert a new message at the next position
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO session_items (session_id, position, item_type, agent_name, message_json, implicit)
		 VALUES (?, (SELECT COALESCE(MAX(position), -1) + 1 FROM session_items WHERE session_id = ?), 'message', ?, ?, ?)`,
		sessionID, sessionID, msg.AgentName, string(msgJSON), msg.Implicit)
	if err != nil {
		return 0, fmt.Errorf("inserting message: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting last insert id: %w", err)
	}

	slog.DebugContext(ctx, "[STORE] AddMessage", "session_id", sessionID, "message_id", id, "role", msg.Message.Role, "agent", msg.AgentName)
	return id, nil
}

// UpdateMessage updates an existing message by its ID.
func (s *SQLiteSessionStore) UpdateMessage(ctx context.Context, messageID int64, msg *Message) error {
	msgJSON, err := json.Marshal(msg.Message)
	if err != nil {
		return fmt.Errorf("marshaling message: %w", err)
	}

	result, err := s.db.ExecContext(ctx,
		`UPDATE session_items SET message_json = ?, implicit = ? WHERE id = ?`,
		string(msgJSON), msg.Implicit, messageID)
	if err != nil {
		return fmt.Errorf("updating message: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return ErrNotFound
	}

	return nil
}

// AddSubSession creates a sub-session and links it to the parent.
func (s *SQLiteSessionStore) AddSubSession(ctx context.Context, parentSessionID string, subSession *Session) error {
	if parentSessionID == "" || subSession.ID == "" {
		return ErrEmptyID
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	// 1. Set parent_id on sub-session
	subSession.ParentID = parentSessionID

	// 2. Insert sub-session as a new session row
	if err := s.addSessionTx(ctx, tx, subSession); err != nil {
		return fmt.Errorf("inserting sub-session: %w", err)
	}

	// 3. Recursively add all items from the sub-session
	for i, item := range subSession.Messages {
		if err := s.addItemTx(ctx, tx, subSession.ID, i, item); err != nil {
			return fmt.Errorf("inserting sub-session item %d: %w", i, err)
		}
	}

	// 4. Add reference in parent's items
	_, err = tx.ExecContext(ctx,
		`INSERT INTO session_items (session_id, position, item_type, subsession_id)
		 VALUES (?, (SELECT COALESCE(MAX(position), -1) + 1 FROM session_items WHERE session_id = ?), 'subsession', ?)`,
		parentSessionID, parentSessionID, subSession.ID)
	if err != nil {
		return fmt.Errorf("inserting subsession reference: %w", err)
	}

	return tx.Commit()
}

// addSessionTx inserts a session within a transaction.
func (s *SQLiteSessionStore) addSessionTx(ctx context.Context, tx *sql.Tx, session *Session) error {
	fields, err := sessionPersistedFieldsOf(session)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO sessions (
			id, tools_approved, safety_policy, input_tokens, output_tokens, title, cost, send_user_message,
			max_iterations, working_dir, created_at, starred, permissions, agent_model_overrides,
			custom_models_used, thinking, parent_id, instruction_context, attributes
		)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.ID, session.ToolsApproved, string(session.SafetyPolicy), session.InputTokens, session.OutputTokens,
		session.Title, session.Cost, session.SendUserMessage, session.MaxIterations,
		session.WorkingDir, session.CreatedAt.Format(time.RFC3339), session.Starred,
		fields.PermissionsJSON, fields.AgentModelOverridesJSON, fields.CustomModelsUsedJSON, false,
		fields.ParentID, fields.InstructionContextJSON, fields.AttributesJSON)
	return err
}

// addItemTx inserts a session item within a transaction.
func (s *SQLiteSessionStore) addItemTx(ctx context.Context, tx *sql.Tx, sessionID string, position int, item Item) error {
	switch {
	case item.Message != nil:
		msgJSON, err := json.Marshal(item.Message.Message)
		if err != nil {
			return fmt.Errorf("marshaling message: %w", err)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO session_items (session_id, position, item_type, agent_name, message_json, implicit)
			 VALUES (?, ?, 'message', ?, ?, ?)`,
			sessionID, position, item.Message.AgentName, string(msgJSON), item.Message.Implicit)
		return err

	case item.SubSession != nil:
		// Recursively add the sub-session
		subSession := item.SubSession
		subSession.ParentID = sessionID

		if err := s.addSessionTx(ctx, tx, subSession); err != nil {
			return fmt.Errorf("inserting nested sub-session: %w", err)
		}

		for i, subItem := range subSession.Messages {
			if err := s.addItemTx(ctx, tx, subSession.ID, i, subItem); err != nil {
				return fmt.Errorf("inserting nested sub-session item %d: %w", i, err)
			}
		}

		_, err := tx.ExecContext(ctx,
			`INSERT INTO session_items (session_id, position, item_type, subsession_id)
			 VALUES (?, ?, 'subsession', ?)`,
			sessionID, position, subSession.ID)
		return err

	case item.Summary != "":
		usageJSON, err := summaryUsageJSON(item.Usage)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO session_items (session_id, position, item_type, summary_text, first_kept_entry, cost, model, usage_json)
			 VALUES (?, ?, 'summary', ?, ?, ?, ?, ?)`,
			sessionID, position, item.Summary, item.FirstKeptEntry, item.Cost, item.Model, usageJSON)
		return err

	case item.Error != nil:
		errJSON, err := json.Marshal(item.Error)
		if err != nil {
			return fmt.Errorf("marshaling error: %w", err)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO session_items (session_id, position, item_type, message_json)
			 VALUES (?, ?, 'error', ?)`,
			sessionID, position, string(errJSON))
		return err

	case item.Termination != nil:
		// Terminations reuse the message_json column like error items;
		// item_type discriminates the row, so no schema migration is needed.
		termJSON, err := json.Marshal(item.Termination)
		if err != nil {
			return fmt.Errorf("marshaling termination: %w", err)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO session_items (session_id, position, item_type, message_json)
			 VALUES (?, ?, 'termination', ?)`,
			sessionID, position, string(termJSON))
		return err

	default:
		return nil // Empty item, skip
	}
}

// AddSummary adds a summary item to a session at the next position.
func (s *SQLiteSessionStore) AddSummary(ctx context.Context, sessionID string, item Item) error {
	if sessionID == "" {
		return ErrEmptyID
	}

	usageJSON, err := summaryUsageJSON(item.Usage)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO session_items (session_id, position, item_type, summary_text, first_kept_entry, cost, model, usage_json)
		 VALUES (?, (SELECT COALESCE(MAX(position), -1) + 1 FROM session_items WHERE session_id = ?), 'summary', ?, ?, ?, ?, ?)`,
		sessionID, sessionID, item.Summary, item.FirstKeptEntry, item.Cost, item.Model, usageJSON)
	if err != nil {
		return err
	}

	return nil
}

// summaryUsageJSON serializes a summary item's usage for the usage_json
// column; nil usage maps to the empty string so old rows and unbilled
// summaries look the same on load.
func summaryUsageJSON(usage *chat.Usage) (string, error) {
	if usage == nil {
		return "", nil
	}
	b, err := json.Marshal(usage)
	if err != nil {
		return "", fmt.Errorf("marshaling summary usage: %w", err)
	}
	return string(b), nil
}

// AddError appends a recorded error item to a session at the next position.
// The error payload is stored as JSON in the message_json column, reusing the
// existing schema (item_type discriminates the row).
func (s *SQLiteSessionStore) AddError(ctx context.Context, sessionID string, e *Error) error {
	if sessionID == "" {
		return ErrEmptyID
	}

	errJSON, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshaling error: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO session_items (session_id, position, item_type, message_json)
		 VALUES (?, (SELECT COALESCE(MAX(position), -1) + 1 FROM session_items WHERE session_id = ?), 'error', ?)`,
		sessionID, sessionID, string(errJSON))
	return err
}

// UpdateSessionTokens updates only token/cost fields.
func (s *SQLiteSessionStore) UpdateSessionTokens(ctx context.Context, sessionID string, inputTokens, outputTokens int64, cost float64) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	_, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET input_tokens = ?, output_tokens = ?, cost = ? WHERE id = ?",
		inputTokens, outputTokens, cost, sessionID)
	return err
}

// UpdateSessionTitle updates only the title.
func (s *SQLiteSessionStore) UpdateSessionTitle(ctx context.Context, sessionID, title string) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	_, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET title = ? WHERE id = ?",
		title, sessionID)
	return err
}
