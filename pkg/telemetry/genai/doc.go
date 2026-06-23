// Package genai provides OpenTelemetry instrumentation helpers that follow
// the GenAI semantic conventions
// (https://opentelemetry.io/docs/specs/semconv/gen-ai/).
//
// The package is structured so that callers — provider clients, the agent
// runtime, MCP clients — describe what they are doing in domain terms and
// the helpers produce the spec-conformant spans, metrics, and log records.
// Centralising the OTel surface here lets us upgrade the semantic
// conventions in one place and keeps the call sites compact.
//
// All gen_ai.* attributes are Development stability per the spec. Attribute
// keys are declared as constants in this package rather than imported from
// go.opentelemetry.io/otel/semconv to insulate callers from the upstream
// reorganisations the GenAI conventions are still going through.
package genai
