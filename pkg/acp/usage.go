package acp

import (
	"math"

	"github.com/docker/docker-agent/pkg/runtime"
)

// recordUsage replaces cumulative costs; only the root owns the context gauge.
func (s *Session) recordUsage(event *runtime.TokenUsageEvent) *runtime.Usage {
	if event == nil || event.Usage == nil || event.SessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess == nil {
		return nil
	}
	s.initUsageCosts()
	s.recordCost(event.SessionID, event.Usage.Cost)
	if event.SessionID == s.sess.ID {
		usage := *event.Usage
		if usage.ContextLength < 0 || usage.ContextLimit < 0 {
			return nil
		}
		s.usageAgent = event.AgentName
		s.contextLimit = usage.ContextLimit
		s.rootUsage = &usage
	}
	if s.rootUsage == nil {
		return nil
	}
	usage := *s.rootUsage
	usage.Cost = s.totalUsageCost()
	return &usage
}

// currentUsage refreshes root counters without a provider lookup.
func (s *Session) currentUsage(agentName string) *runtime.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initUsageCosts()
	limit := s.contextLimit
	if s.usageAgent != agentName {
		limit = 0
	}
	usage := runtime.SessionUsage(s.sess, limit)
	s.recordCost(s.sess.ID, usage.Cost)
	usage.Cost = s.totalUsageCost()
	return usage
}

// Callers hold s.mu; restored descendants are already included in the root entry.
func (s *Session) initUsageCosts() {
	if s.usageCosts == nil {
		s.usageCosts = make(map[string]float64)
		s.recordCost(s.sess.ID, runtime.SessionUsage(s.sess, 0).Cost)
	}
}

func (s *Session) totalUsageCost() float64 {
	var total float64
	for _, cost := range s.usageCosts {
		total = min(math.MaxFloat64, total+cost)
	}
	return total
}

func (s *Session) recordCost(sid string, cost float64) {
	if cost >= 0 && !math.IsNaN(cost) && !math.IsInf(cost, 0) {
		s.usageCosts[sid] = cost
	}
}

func (s *Session) retainUsage(event runtime.Event) {
	if usage, ok := event.(*runtime.TokenUsageEvent); ok {
		s.recordUsage(usage)
	}
}
