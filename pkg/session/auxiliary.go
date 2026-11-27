package session

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/docker/docker-agent/pkg/chat"
)

// AuxiliaryUsage records the cost of a paid side request (e.g. /btw) without
// retaining its prompt or answer, mirroring the transient-record pattern
// already used by [Evaluation].
type AuxiliaryUsage struct {
	ID      string      `json:"id"`
	Purpose string      `json:"purpose"`
	Model   string      `json:"model"`
	Usage   *chat.Usage `json:"usage,omitempty"`
	Cost    *float64    `json:"cost,omitempty"`
}

func cloneAuxiliaryUsage(record *AuxiliaryUsage) *AuxiliaryUsage {
	if record == nil {
		return nil
	}
	cloned := *record
	if record.Usage != nil {
		usage := *record.Usage
		cloned.Usage = &usage
	}
	if record.Cost != nil {
		cost := *record.Cost
		cloned.Cost = &cost
	}
	return &cloned
}

// AddAuxiliaryUsage appends an immutable record once, even when the runtime
// and persistence observer share this session.
func (s *Session) AddAuxiliaryUsage(record *AuxiliaryUsage) {
	if record == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.Messages {
		if item.AuxiliaryUsage != nil && item.AuxiliaryUsage.ID == record.ID {
			return
		}
	}
	s.Messages = append(s.Messages, Item{AuxiliaryUsage: cloneAuxiliaryUsage(record)})
	s.Cost = s.totalCostLocked()
}

// AuxiliaryUsageStore is implemented by session stores that support
// accounting for side requests (e.g. /btw) independently of the
// conversation. Stores that predate the feature simply don't implement it;
// callers type-assert for support.
type AuxiliaryUsageStore interface {
	AddAuxiliaryUsage(ctx context.Context, sessionID string, record *AuxiliaryUsage) error
}

// AddAuxiliaryUsage appends an auxiliary-usage record to sessionID.
func (s *InMemorySessionStore) AddAuxiliaryUsage(_ context.Context, sessionID string, record *AuxiliaryUsage) error {
	sess, ok := s.sessions.Load(sessionID)
	if !ok {
		return ErrNotFound
	}
	sess.AddAuxiliaryUsage(record)
	return nil
}

// AddAuxiliaryUsage stores the payload in message_json, deduplicating by ID
// like AddEvaluation.
func (s *SQLiteSessionStore) AddAuxiliaryUsage(ctx context.Context, sessionID string, record *AuxiliaryUsage) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	if record == nil {
		return nil
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshaling auxiliary usage: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO session_items (session_id, position, item_type, message_json)
		 SELECT ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM session_items WHERE session_id = ?), 'auxiliary_usage', ?
		 WHERE NOT EXISTS (SELECT 1 FROM session_items WHERE session_id = ? AND item_type = 'auxiliary_usage' AND json_extract(message_json, '$.id') = ?)`,
		sessionID, sessionID, string(data), sessionID, record.ID)
	return err
}
