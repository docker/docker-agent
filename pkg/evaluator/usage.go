package evaluator

import "context"

// UsageRecord reports request accounting independently of answer validity.
type UsageRecord struct {
	// Model is the returned ID, or the requested ID when unavailable.
	Model string
	// Usage is nil when token counts are missing or invalid, not when they are zero.
	Usage *Usage
	// Cost is the estimated USD charge; nil means usage or pricing is unknown.
	Cost *float64
}

type usageObserverKey struct{}

// WithUsageObserver attaches a synchronous per-request observer to ctx, replacing
// any inherited observer. A nil observer disables observation. Callbacks shared
// by concurrent evaluations must synchronize their own state.
func WithUsageObserver(ctx context.Context, observer func(UsageRecord)) context.Context {
	return context.WithValue(ctx, usageObserverKey{}, observer)
}

// ObserveUsage delivers an accounting record to the observer on ctx.
// Providers call it once per attempted HTTP request, even when answer validation fails.
// All callbacks must finish before Evaluate returns.
func ObserveUsage(ctx context.Context, record UsageRecord) {
	if observer, _ := ctx.Value(usageObserverKey{}).(func(UsageRecord)); observer != nil {
		// Keep observer mutations independent of the returned assessment.
		if record.Usage != nil {
			usage := *record.Usage
			record.Usage = &usage
		}
		if record.Cost != nil {
			cost := *record.Cost
			record.Cost = &cost
		}
		observer(record)
	}
}
