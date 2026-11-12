package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/evaluator"
)

func TestEvaluatorJudgeCheck(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		p      float64
		passed bool
	}{
		{"zero", 0, false},
		{"below threshold", 0.4999, false},
		{"threshold", 0.5, true},
		{"above threshold", 0.9, true},
		{"one", 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := replayEvaluator(func(_ context.Context, state any) (*evaluator.Result, error) {
				assert.Equal(t, map[string]any{"transcript": "evidence", "criterion": "rubric"}, state)
				return &evaluator.Result{Type: "boolean", Probability: new(tt.p)}, nil
			})
			backend := &evaluatorJudge{client: client}
			verdict, err := backend.Check(t.Context(), "evidence", "rubric")
			require.NoError(t, err)
			assert.Equal(t, tt.passed, verdict.Passed)
			require.NotNil(t, verdict.Probability)
			assert.InDelta(t, tt.p, *verdict.Probability, 1e-9)
			assert.Contains(t, verdict.Reason, "probability")
			assert.Contains(t, verdict.Reason, "pass threshold 0.50")
		})
	}
}

func TestEvaluatorJudgeInvalidResults(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		result *evaluator.Result
	}{
		{"nil result", nil},
		{"missing probability", &evaluator.Result{Type: "boolean"}},
		{"wrong type", &evaluator.Result{Type: "score", Probability: new(0.9)}},
		{"negative", &evaluator.Result{Type: "boolean", Probability: new(-0.1)}},
		{"greater than one", &evaluator.Result{Type: "boolean", Probability: new(1.1)}},
		{"nan", &evaluator.Result{Type: "boolean", Probability: new(math.NaN())}},
		{"infinite", &evaluator.Result{Type: "boolean", Probability: new(math.Inf(1))}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backend := &evaluatorJudge{client: replayEvaluator(func(context.Context, any) (*evaluator.Result, error) {
				return tt.result, nil
			})}
			_, err := backend.Check(t.Context(), "transcript", "criterion")
			require.Error(t, err)
		})
	}
}

func TestEvaluatorJudgeErrors(t *testing.T) {
	t.Parallel()

	for _, expected := range []error{errors.New("provider failure"), context.Canceled, context.DeadlineExceeded} {
		t.Run(expected.Error(), func(t *testing.T) {
			t.Parallel()
			backend := &evaluatorJudge{client: replayEvaluator(func(context.Context, any) (*evaluator.Result, error) {
				return nil, expected
			})}
			judge := NewJudgeWithBackend(backend, 2)
			results, err := judge.CheckRelevance(t.Context(), "transcript", []string{"first", "second"})
			require.ErrorIs(t, err, expected)
			assert.Len(t, results, 2)
			assert.False(t, results[0].Passed)
			assert.Nil(t, results[0].Probability)
		})
	}
}

func TestEvaluatorJudgeUsageObserver(t *testing.T) {
	t.Parallel()

	var records []evaluator.UsageRecord
	ctx := evaluator.WithUsageObserver(t.Context(), func(record evaluator.UsageRecord) {
		records = append(records, record)
	})
	backend := &evaluatorJudge{client: replayEvaluator(func(ctx context.Context, _ any) (*evaluator.Result, error) {
		evaluator.ObserveUsage(ctx, evaluator.UsageRecord{Model: "test", Cost: new(0.01)})
		return &evaluator.Result{Type: "boolean", Probability: new(0.9)}, nil
	})}
	_, err := backend.Check(ctx, "transcript", "criterion")
	require.NoError(t, err)
	assert.Len(t, records, 1)
}

func TestRelevanceResultProbabilityJSON(t *testing.T) {
	t.Parallel()

	for _, probability := range []*float64{nil, new(0.0), new(0.9)} {
		result := RelevanceResult{Criterion: "criterion", Probability: probability}
		data, err := json.Marshal(result)
		require.NoError(t, err)
		if probability == nil {
			assert.NotContains(t, string(data), "probability")
		} else {
			assert.Contains(t, string(data), "probability")
		}
		var decoded RelevanceResult
		require.NoError(t, json.Unmarshal(data, &decoded))
		assert.Equal(t, result, decoded)
	}
}
