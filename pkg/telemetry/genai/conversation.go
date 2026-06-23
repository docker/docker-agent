package genai

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
)

// baggageKeyConversationID matches the GenAI semconv attribute key for
// the conversation identifier so the value flows transparently through
// the W3C `baggage` header alongside `traceparent`. Any downstream
// service or subprocess running OTel auto-instrumentation will pick it
// up without per-helper plumbing.
const baggageKeyConversationID = "gen_ai.conversation.id"

// WithConversationID returns a context that carries the conversation id
// in OTel baggage. Spans created later in the chain — including ones in
// helper packages that have no direct access to the session — read it
// via ConversationIDFromContext and attach `gen_ai.conversation.id`
// automatically. Empty id is a no-op.
func WithConversationID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	member, err := baggage.NewMember(baggageKeyConversationID, id)
	if err != nil {
		return ctx
	}
	bag, err := baggage.FromContext(ctx).SetMember(member)
	if err != nil {
		return ctx
	}
	return baggage.ContextWithBaggage(ctx, bag)
}

// ConversationIDFromContext returns the conversation id stored in the
// context's baggage, or "" when none has been seeded.
func ConversationIDFromContext(ctx context.Context) string {
	return baggage.FromContext(ctx).Member(baggageKeyConversationID).Value()
}

// conversationAttribute returns the gen_ai.conversation.id attribute
// from baggage when present, or zero-value KeyValue when absent. Helper
// for span starters so they can append it in one line.
func conversationAttribute(ctx context.Context) (attribute.KeyValue, bool) {
	id := ConversationIDFromContext(ctx)
	if id == "" {
		return attribute.KeyValue{}, false
	}
	return attribute.String(AttrConversationID, id), true
}
