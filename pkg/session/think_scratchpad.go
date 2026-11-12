package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// ScratchpadKey separates conversations, callers and configured think instances.
type ScratchpadKey struct {
	SessionID  string
	AgentName  string
	ToolsetKey string
}

// ThinkScratchpadStore atomically appends a thought and returns an independent
// ordered snapshot. rootID owns cleanup, not the scratchpad's sharing identity.
type ThinkScratchpadStore interface {
	AppendThought(ctx context.Context, rootID string, key ScratchpadKey, thought string) ([]string, error)
}

var ErrScratchpadOwner = errors.New("think scratchpad root missing or ownership inconsistent")

func validateScratchpadKey(rootID string, key ScratchpadKey) error {
	if rootID == "" || key.SessionID == "" {
		return ErrEmptyID
	}
	if key.AgentName == "" || key.ToolsetKey == "" {
		return ErrScratchpadOwner
	}
	return nil
}

func (s *InMemorySessionStore) AppendThought(ctx context.Context, rootID string, key ScratchpadKey, thought string) ([]string, error) {
	if err := validateScratchpadKey(rootID, key); err != nil {
		return nil, err
	}
	s.scratchpadMu.Lock()
	defer s.scratchpadMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, exists := s.sessions.Load(rootID)
	if !exists {
		return nil, ErrScratchpadOwner
	}
	root.mu.RLock()
	isRoot := root.ParentID == ""
	root.mu.RUnlock()
	if !isRoot {
		return nil, ErrScratchpadOwner
	}
	if owner, exists := s.scratchpadOwners[key.SessionID]; exists && owner != rootID {
		return nil, ErrScratchpadOwner
	}
	if s.scratchpadOwners == nil {
		s.scratchpadOwners = make(map[string]string)
	}
	if s.scratchpads == nil {
		s.scratchpads = make(map[string]map[ScratchpadKey][]string)
	}
	if s.scratchpads[rootID] == nil {
		s.scratchpads[rootID] = make(map[ScratchpadKey][]string)
	}
	thoughts := append(s.scratchpads[rootID][key], thought)
	s.scratchpads[rootID][key] = thoughts
	s.scratchpadOwners[key.SessionID] = rootID
	return slices.Clone(thoughts), nil
}

func (s *SQLiteSessionStore) AppendThought(ctx context.Context, rootID string, key ScratchpadKey, thought string) ([]string, error) {
	if err := validateScratchpadKey(rootID, key); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin think append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Acquire the write lock before reading: independently loaded sessions must
	// not upgrade stale read transactions or allocate the same sequence.
	result, err := tx.ExecContext(ctx, `
		INSERT INTO think_scratchpad (root_id, session_id, agent_name, toolset_key, seq, thought)
		SELECT ?, ?, ?, ?, COALESCE((
			SELECT MAX(seq) FROM think_scratchpad WHERE session_id = ? AND agent_name = ? AND toolset_key = ?
		), 0) + 1, ?
		WHERE EXISTS (SELECT 1 FROM sessions WHERE id = ? AND (parent_id IS NULL OR parent_id = ''))
		AND NOT EXISTS (
			SELECT 1 FROM think_scratchpad WHERE session_id = ? AND root_id != ?
		)`, rootID, key.SessionID, key.AgentName, key.ToolsetKey,
		key.SessionID, key.AgentName, key.ToolsetKey, thought, rootID,
		key.SessionID, rootID)
	if err != nil {
		return nil, fmt.Errorf("append think thought: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, ErrScratchpadOwner
	}
	rows, err := tx.QueryContext(ctx, `SELECT thought FROM think_scratchpad
		WHERE session_id = ? AND agent_name = ? AND toolset_key = ? ORDER BY seq`,
		key.SessionID, key.AgentName, key.ToolsetKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var thoughts []string
	for rows.Next() {
		var thought string
		if err := rows.Scan(&thought); err != nil {
			return nil, err
		}
		thoughts = append(thoughts, thought)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit think thought: %w", err)
	}
	return thoughts, nil
}
