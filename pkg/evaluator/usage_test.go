package evaluator

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUsageObserverContext(t *testing.T) {
	t.Parallel()

	var outer, inner []UsageRecord
	parent := WithUsageObserver(t.Context(), func(record UsageRecord) { outer = append(outer, record) })
	child := WithUsageObserver(parent, func(record UsageRecord) { inner = append(inner, record) })
	record := UsageRecord{Model: "resolved", Usage: &Usage{InputTokens: 12}}
	ObserveUsage(t.Context(), record)
	ObserveUsage(WithUsageObserver(parent, nil), record)
	ObserveUsage(child, record)
	assert.Empty(t, outer)
	require.Len(t, inner, 1)
	assert.Equal(t, record, inner[0])
	ObserveUsage(parent, record)
	require.Len(t, outer, 1)
	assert.Equal(t, record, outer[0])
}

func TestUsageObserverCopiesValues(t *testing.T) {
	t.Parallel()

	cost := 1.0
	record := UsageRecord{Usage: &Usage{InputTokens: 12, OutputTokens: 3}, Cost: &cost}
	ctx := WithUsageObserver(t.Context(), func(record UsageRecord) {
		record.Usage.InputTokens = 100
		*record.Cost = 100
	})
	ObserveUsage(ctx, record)
	assert.EqualValues(t, 12, record.Usage.InputTokens)
	assert.InDelta(t, 1, *record.Cost, 1e-12)
}

func TestResultCostJSON(t *testing.T) {
	t.Parallel()

	result := Result{}
	data, err := json.Marshal(result)
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"cost"`)
	zero := 0.0
	result.Cost = &zero
	data, err = json.Marshal(result)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"cost":0`)
}
