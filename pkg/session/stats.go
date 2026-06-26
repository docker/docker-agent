package session

import (
	"cmp"
	"slices"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
)

// Stats aggregates high-level interaction and usage metrics for a session.
// It is derived data computed by walking the session and its sub-sessions; it
// is never persisted and carries no billing authority. It backs end-of-run
// summaries such as the non-interactive (--exec) recap.
type Stats struct {
	// ID is the session identifier.
	ID string

	// Duration is the wall-clock span between the first and last message,
	// approximated from message timestamps (see Session.Duration).
	Duration time.Duration

	// Requests is the number of assistant responses that reported usage.
	Requests int

	// ToolCalls is the number of completed tool calls (tool-role results).
	ToolCalls int

	// ToolErrors is the number of completed tool calls that failed.
	ToolErrors int

	// InputTokens counts new (uncached) input tokens.
	InputTokens int64

	// CachedInput counts input tokens served from cache (cache reads).
	CachedInput int64

	// CacheWrite counts input tokens written to the cache.
	CacheWrite int64

	// OutputTokens counts generated output tokens.
	OutputTokens int64

	// ReasoningTokens counts reasoning tokens (subset of output for some models).
	ReasoningTokens int64

	// Cost is the total session cost in dollars.
	Cost float64

	// Models holds the per-model breakdown, sorted by cost descending.
	Models []ModelStats
}

// ModelStats holds the per-model usage breakdown within a session.
type ModelStats struct {
	Model        string
	Requests     int
	InputTokens  int64 // new (uncached) input tokens
	CachedInput  int64 // cache reads
	OutputTokens int64
	Cost         float64
}

// ToolSuccesses returns the number of completed tool calls that succeeded.
func (s Stats) ToolSuccesses() int { return s.ToolCalls - s.ToolErrors }

// SuccessRate returns the tool-call success rate as a percentage in [0,100].
// It returns 0 when no tool calls were made.
func (s Stats) SuccessRate() float64 {
	if s.ToolCalls == 0 {
		return 0
	}
	return float64(s.ToolCalls-s.ToolErrors) / float64(s.ToolCalls) * 100
}

// TotalInput returns all input tokens, including cache reads and writes.
func (s Stats) TotalInput() int64 { return s.InputTokens + s.CachedInput + s.CacheWrite }

// CacheHitRate returns the share of input tokens served from cache as a
// percentage in [0,100]. It returns 0 when there is no countable input.
func (s Stats) CacheHitRate() float64 {
	candidate := s.InputTokens + s.CachedInput
	if candidate == 0 {
		return 0
	}
	return float64(s.CachedInput) / float64(candidate) * 100
}

// HasActivity reports whether the session did anything worth summarizing.
func (s Stats) HasActivity() bool {
	return s.Requests > 0 || s.ToolCalls > 0 || s.Cost > 0
}

// Stats computes a Stats snapshot for the session. It walks sub-sessions so
// that delegated work (task transfers) is included in the totals, mirroring
// TotalCost's accounting.
func (s *Session) Stats() Stats {
	// Duration takes its own lock; compute it before walking so we never
	// hold a read lock across the call.
	st := Stats{
		ID:       s.ID,
		Duration: s.Duration(),
	}

	modelIndex := make(map[string]int)

	var walk func(sess *Session)
	walk = func(sess *Session) {
		for _, item := range sess.snapshotItems() {
			switch {
			case item.IsMessage():
				m := item.Message.Message
				st.Cost += m.Cost
				switch m.Role {
				case chat.MessageRoleTool:
					st.ToolCalls++
					if m.IsError {
						st.ToolErrors++
					}
				case chat.MessageRoleAssistant:
					if m.Usage != nil {
						st.Requests++
						st.InputTokens += m.Usage.InputTokens
						st.CachedInput += m.Usage.CachedInputTokens
						st.CacheWrite += m.Usage.CacheWriteTokens
						st.OutputTokens += m.Usage.OutputTokens
						st.ReasoningTokens += m.Usage.ReasoningTokens

						model := cmp.Or(m.Model, "unknown")
						idx, ok := modelIndex[model]
						if !ok {
							idx = len(st.Models)
							modelIndex[model] = idx
							st.Models = append(st.Models, ModelStats{Model: model})
						}
						ms := &st.Models[idx]
						ms.Requests++
						ms.InputTokens += m.Usage.InputTokens
						ms.CachedInput += m.Usage.CachedInputTokens
						ms.OutputTokens += m.Usage.OutputTokens
						ms.Cost += m.Cost
					}
				}
			case item.IsSubSession():
				walk(item.SubSession)
			}
			// Item-level cost (e.g. compaction/summarization) lives outside any
			// message, so it is added once per item, matching TotalCost.
			st.Cost += item.Cost
		}
	}
	walk(s)

	// Remote mode keeps per-message usage in MessageUsageHistory rather than in
	// the message list, so fall back to it when the walk found no usage.
	if st.Requests == 0 && len(s.MessageUsageHistory) > 0 {
		for _, r := range s.MessageUsageHistory {
			st.Requests++
			st.InputTokens += r.Usage.InputTokens
			st.CachedInput += r.Usage.CachedInputTokens
			st.CacheWrite += r.Usage.CacheWriteTokens
			st.OutputTokens += r.Usage.OutputTokens
			st.ReasoningTokens += r.Usage.ReasoningTokens
			st.Cost += r.Cost

			model := cmp.Or(r.Model, "unknown")
			idx, ok := modelIndex[model]
			if !ok {
				idx = len(st.Models)
				modelIndex[model] = idx
				st.Models = append(st.Models, ModelStats{Model: model})
			}
			ms := &st.Models[idx]
			ms.Requests++
			ms.InputTokens += r.Usage.InputTokens
			ms.CachedInput += r.Usage.CachedInputTokens
			ms.OutputTokens += r.Usage.OutputTokens
			ms.Cost += r.Cost
		}
	}

	slices.SortFunc(st.Models, func(a, b ModelStats) int {
		if c := cmp.Compare(b.Cost, a.Cost); c != 0 {
			return c
		}
		return cmp.Compare(a.Model, b.Model)
	})

	return st
}
