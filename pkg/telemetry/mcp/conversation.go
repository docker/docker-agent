package mcp

import (
	"context"

	"go.opentelemetry.io/otel/baggage"
)

// ConversationIDFromBaggage reads `gen_ai.conversation.id` from the
// context's W3C baggage. The MCP package mirrors the genai package's
// convention so MCP spans automatically carry the session id when the
// runtime has seeded it; the value also propagates across MCP server
// boundaries via the standard `baggage` header alongside `traceparent`.
//
// Exported so adjacent code (e.g. the MCP OAuth transport) can attach
// the same attribute to spans it creates directly via `otel.Tracer`.
func ConversationIDFromBaggage(ctx context.Context) string {
	return baggage.FromContext(ctx).Member("gen_ai.conversation.id").Value()
}
