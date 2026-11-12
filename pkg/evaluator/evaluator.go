// Package evaluator provides typed assessments without applying decision policies.
package evaluator

import "context"

// Evaluator assesses a string, JSON object, or JSON array.
type Evaluator interface {
	Evaluate(ctx context.Context, state any) (*Result, error)
}

// Result is an assessment; callers decide how its probabilities affect behavior.
type Result struct {
	Type          string             `json:"type"`
	Model         string             `json:"model"`
	Probability   *float64           `json:"probability,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Usage         Usage              `json:"usage"`
	// Cost is the estimated USD charge; nil means usage or pricing is unknown.
	Cost *float64 `json:"cost,omitempty"`
}

// Usage records the tokens consumed by an evaluation.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}
