package genai

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// memoryLogExporter collects exported log records so tests can inspect
// what EmitEvaluationResult produced.
type memoryLogExporter struct {
	records []sdklog.Record
}

func (e *memoryLogExporter) Export(_ context.Context, records []sdklog.Record) error {
	// The SDK may reuse the slice; clone before retaining.
	for _, r := range records {
		e.records = append(e.records, r.Clone())
	}
	return nil
}

func (e *memoryLogExporter) Shutdown(context.Context) error   { return nil }
func (e *memoryLogExporter) ForceFlush(context.Context) error { return nil }

// installRecordingLogger replaces the global OTel logger provider with an
// in-memory SDK provider for the duration of the test and returns the
// exporter so callers can inspect emitted records.
func installRecordingLogger(t *testing.T) *memoryLogExporter {
	t.Helper()
	exp := &memoryLogExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	prev := global.GetLoggerProvider()
	global.SetLoggerProvider(lp)
	t.Cleanup(func() {
		_ = lp.Shutdown(t.Context())
		global.SetLoggerProvider(prev)
	})
	return exp
}

func recordAttributes(rec sdklog.Record) map[attribute.Key]attribute.Value {
	attrs := make(map[attribute.Key]attribute.Value, rec.AttributesLen())
	rec.WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[kv.Key] = kv.Value
		return true
	})
	return attrs
}

// Mutates the global OTel logger provider; cannot run in parallel.
func TestEmitEvaluationResult(t *testing.T) {
	exp := installRecordingLogger(t)

	ctx := WithConversationID(t.Context(), "conv-42")
	EmitEvaluationResult(ctx, EvaluationResult{
		Name:          "relevance",
		ScoreLabel:    "passed",
		ScoreValue:    0.9,
		HasScoreValue: true,
		Explanation:   "on topic",
		ErrorType:     "timeout",
	})

	require.Len(t, exp.records, 1)
	rec := exp.records[0]
	assert.Equal(t, "gen_ai.evaluation.result", rec.EventName())
	assert.Equal(t, log.SeverityInfo, rec.Severity())
	assert.Equal(t, "INFO", rec.SeverityText())

	attrs := recordAttributes(rec)
	assert.Equal(t, attribute.StringValue("relevance"), attrs[AttrEvaluationName])
	assert.Equal(t, attribute.StringValue("passed"), attrs[AttrEvaluationScoreLabel])
	assert.Equal(t, attribute.Float64Value(0.9), attrs[AttrEvaluationScoreValue])
	assert.Equal(t, attribute.StringValue("on topic"), attrs[AttrEvaluationExplanation])
	assert.Equal(t, attribute.StringValue("timeout"), attrs["error.type"])
	assert.Equal(t, attribute.StringValue("conv-42"), attrs[AttrConversationID])
}

// Mutates the global OTel logger provider; cannot run in parallel.
func TestEmitEvaluationResult_OptionalFieldsOmitted(t *testing.T) {
	exp := installRecordingLogger(t)

	EmitEvaluationResult(t.Context(), EvaluationResult{Name: "factuality"})

	require.Len(t, exp.records, 1)
	attrs := recordAttributes(exp.records[0])
	assert.Equal(t, attribute.StringValue("factuality"), attrs[AttrEvaluationName])
	assert.NotContains(t, attrs, attribute.Key(AttrEvaluationScoreLabel))
	assert.NotContains(t, attrs, attribute.Key(AttrEvaluationScoreValue))
	assert.NotContains(t, attrs, attribute.Key(AttrEvaluationExplanation))
	assert.NotContains(t, attrs, attribute.Key("error.type"))
	assert.NotContains(t, attrs, attribute.Key(AttrConversationID))
}
