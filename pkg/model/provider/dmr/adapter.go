package dmr

import (
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/oaistream"
)

// newStreamAdapter returns the shared OpenAI stream adapter implementation
func newStreamAdapter(stream *ssestream.Stream[openai.ChatCompletionChunk], trackUsage bool) chat.MessageStream {
	return oaistream.NewStreamAdapter(stream, trackUsage)
}
