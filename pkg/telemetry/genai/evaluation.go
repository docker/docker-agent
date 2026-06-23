package genai

import (
	"context"

	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
)

// EvaluationResult describes one evaluation outcome that should be emitted
// as a `gen_ai.evaluation.result` log record per the OTel GenAI semconv
// (https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-events/).
type EvaluationResult struct {
	// Name is the evaluation metric — e.g. "relevance", "factuality",
	// "tool_calls_f1". Required.
	Name string

	// ScoreLabel is the human-readable verdict — e.g. "passed",
	// "failed", "satisfactory". Optional but commonly set.
	ScoreLabel string

	// ScoreValue is the numeric score (commonly 0.0–1.0). Optional.
	ScoreValue    float64
	HasScoreValue bool

	// Explanation is a free-form reason for the score. Optional.
	Explanation string

	// ErrorType is set when the evaluation itself failed (e.g. the
	// judge model errored out). Mirrors the spec's `error.type` field.
	ErrorType string
}

// EmitEvaluationResult emits a `gen_ai.evaluation.result` log record. The
// record links to the active span via the supplied context so dashboards
// can join evaluation outcomes back onto the operation that produced
// them. No-op when no logger provider is configured.
func EmitEvaluationResult(ctx context.Context, result EvaluationResult) {
	logger := global.GetLoggerProvider().Logger(instrumentationName)

	var rec log.Record
	rec.SetEventName("gen_ai.evaluation.result")
	rec.SetSeverity(log.SeverityInfo)
	rec.SetSeverityText("INFO")

	rec.AddAttributes(log.String(AttrEvaluationName, result.Name))
	if result.ScoreLabel != "" {
		rec.AddAttributes(log.String(AttrEvaluationScoreLabel, result.ScoreLabel))
	}
	if result.HasScoreValue {
		rec.AddAttributes(log.Float64(AttrEvaluationScoreValue, result.ScoreValue))
	}
	if result.Explanation != "" {
		rec.AddAttributes(log.String(AttrEvaluationExplanation, result.Explanation))
	}
	if result.ErrorType != "" {
		rec.AddAttributes(log.String("error.type", result.ErrorType))
	}
	if convID := ConversationIDFromContext(ctx); convID != "" {
		rec.AddAttributes(log.String(AttrConversationID, convID))
	}

	logger.Emit(ctx, rec)
}
