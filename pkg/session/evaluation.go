package session

import (
	"slices"

	"github.com/docker/docker-agent/pkg/chat"
)

// Evaluation records one evaluator request without adding to the chat context.
type Evaluation struct {
	ID        string      `json:"id"`
	Evaluator string      `json:"evaluator"`
	AgentName string      `json:"agent_name"`
	Model     string      `json:"model"`
	Usage     *chat.Usage `json:"usage,omitempty"`
	Cost      *float64    `json:"cost,omitempty"`
	CreatedAt string      `json:"created_at"`
}

func cloneEvaluation(e *Evaluation) *Evaluation {
	if e == nil {
		return nil
	}
	cloned := *e
	if e.Usage != nil {
		usage := *e.Usage
		cloned.Usage = &usage
	}
	if e.Cost != nil {
		cost := *e.Cost
		cloned.Cost = &cost
	}
	return &cloned
}

func cloneEvaluations(evaluations []*Evaluation) []*Evaluation {
	cloned := slices.Clone(evaluations)
	for i, e := range cloned {
		cloned[i] = cloneEvaluation(e)
	}
	return cloned
}

// AddEvaluation appends an immutable record once, even when the runtime and
// persistence observer share this session.
func (s *Session) AddEvaluation(e *Evaluation) {
	if e == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.Messages {
		if item.Evaluation != nil && item.Evaluation.ID == e.ID {
			return
		}
	}
	s.Messages = append(s.Messages, Item{Evaluation: cloneEvaluation(e)})
	s.Cost = s.totalCostLocked()
}

// persistenceCostLocked preserves legacy scalar costs unless evaluator items are present.
// The caller must hold s.mu.
func (s *Session) persistenceCostLocked() float64 {
	if s.hasEvaluationsLocked() {
		return s.totalCostLocked()
	}
	return s.Cost
}

func (s *Session) hasEvaluationsLocked() bool {
	for _, item := range s.Messages {
		if item.Evaluation != nil {
			return true
		}
		if item.IsSubSession() {
			child := item.SubSession
			child.mu.RLock()
			found := child.hasEvaluationsLocked()
			child.mu.RUnlock()
			if found {
				return true
			}
		}
	}
	return false
}

// syncEvaluationCosts normalizes imported trees retained by the in-memory store.
func (s *Session) syncEvaluationCosts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.Messages {
		if item.IsSubSession() {
			item.SubSession.syncEvaluationCosts()
		}
	}
	s.Cost = s.persistenceCostLocked()
}

// AddEvaluationUsageRecord retains evaluator events independently of persisted items.
func (s *Session) AddEvaluationUsageRecord(e *Evaluation) {
	if e == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.ContainsFunc(s.EvaluationUsageHistory, func(record *Evaluation) bool {
		return record != nil && record.ID == e.ID
	}) {
		return
	}
	s.EvaluationUsageHistory = append(s.EvaluationUsageHistory, cloneEvaluation(e))
}

// EvaluationUsageHistorySnapshot returns deep copies of this session's transient records.
func (s *Session) EvaluationUsageHistorySnapshot() []*Evaluation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneEvaluations(s.EvaluationUsageHistory)
}

// EvaluationUsageHistoryCount counts transient records throughout the session tree.
func (s *Session) EvaluationUsageHistoryCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	n := len(s.EvaluationUsageHistory)
	for _, item := range s.Messages {
		if item.IsSubSession() {
			n += item.SubSession.EvaluationUsageHistoryCount()
		}
	}
	return n
}
