// Package mcp provides OpenTelemetry instrumentation helpers that follow
// the OTel GenAI semantic conventions for the Model Context Protocol
// (https://opentelemetry.io/docs/specs/semconv/gen-ai/mcp/).
//
// MCP attributes use the `mcp.*` namespace (separate from `gen_ai.*`).
// Trace context propagates through the MCP `params._meta` field so that
// requests crossing client/server boundaries chain into a single trace.
//
// The package is structured so that callers describe what they are doing
// in MCP terms (method name, tool name, session id) and the helpers
// produce the spec-conformant spans, metrics, and propagation. All helpers
// are no-op-safe when telemetry is disabled.
package mcp
