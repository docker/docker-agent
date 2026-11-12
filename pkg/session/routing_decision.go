package session

import (
	"time"
)

// RoutingDecision records a control-hook assessment and its resulting route.
// It deliberately excludes task input, outputs, and conversation content.
type RoutingDecision struct {
	ID             string    `json:"id"`
	InvocationID   string    `json:"invocation_id"`
	StepID         string    `json:"step_id"`
	Phase          string    `json:"phase"`
	FromAgent      string    `json:"from_agent"`
	ToAgent        string    `json:"to_agent,omitempty"`
	Action         string    `json:"action"`
	Evaluator      string    `json:"evaluator,omitempty"`
	Selected       string    `json:"selected,omitempty"`
	Probability    *float64  `json:"probability,omitempty"`
	Model          string    `json:"model,omitempty"`
	FallbackReason string    `json:"fallback_reason,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

func cloneRoutingDecision(decision *RoutingDecision) *RoutingDecision {
	if decision == nil {
		return nil
	}
	cloned := *decision
	if decision.Probability != nil {
		probability := *decision.Probability
		cloned.Probability = &probability
	}
	return &cloned
}

// AddRoutingDecision appends an immutable decision record once.
func (s *Session) AddRoutingDecision(decision *RoutingDecision) {
	if decision == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.Messages {
		if item.RoutingDecision != nil && item.RoutingDecision.ID == decision.ID {
			return
		}
	}
	s.Messages = append(s.Messages, Item{RoutingDecision: cloneRoutingDecision(decision)})
}

// RoutingDecisionHistory returns copies of control decisions in transcript order.
func (s *Session) RoutingDecisionHistory() []*RoutingDecision {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var decisions []*RoutingDecision
	for _, item := range s.Messages {
		if item.RoutingDecision != nil {
			decisions = append(decisions, cloneRoutingDecision(item.RoutingDecision))
		}
	}
	return decisions
}
