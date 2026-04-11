package openai

import (
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"

	"github.com/docker/docker-agent/pkg/chat"
)

// ConvertMessagesToResponseInput exposes the shared Responses API message conversion.
func ConvertMessagesToResponseInput(messages []chat.Message) []responses.ResponseInputItemUnionParam {
	return convertMessagesToResponseInput(messages)
}

// NewResponseSSEAdapter exposes the shared Responses API SSE stream adapter.
func NewResponseSSEAdapter(stream *ssestream.Stream[responses.ResponseStreamEventUnion], trackUsage bool) chat.MessageStream {
	return newResponseStreamAdapter(stream, trackUsage)
}
