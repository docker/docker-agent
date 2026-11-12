package a2a

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/servesafety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// stallingStream yields its chunks and then parks until the model call is
// cancelled, standing in for a provider that only stops on cancellation.
type stallingStream struct {
	ctx    context.Context //nolint:containedctx // the model-call context, inspected after the adapter returns
	chunks []string
	closed atomic.Bool
}

func (s *stallingStream) Recv() (chat.MessageStreamResponse, error) {
	if len(s.chunks) > 0 {
		chunk := s.chunks[0]
		s.chunks = s.chunks[1:]
		return chat.MessageStreamResponse{
			Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: chunk}}},
		}, nil
	}
	<-s.ctx.Done()
	return chat.MessageStreamResponse{}, s.ctx.Err()
}

func (s *stallingStream) Close() { s.closed.Store(true) }

type stallingProvider struct {
	chunks []string

	mu     sync.Mutex
	stream *stallingStream
}

func (p *stallingProvider) ID() modelsdev.ID { return modelsdev.NewID("test", "mock-model") }

func (p *stallingProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stream = &stallingStream{ctx: ctx, chunks: p.chunks}
	return p.stream, nil
}

func (p *stallingProvider) BaseConfig() base.Config { return base.Config{} }

func (p *stallingProvider) MaxTokens() int { return 0 }

func (p *stallingProvider) current() *stallingStream {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stream
}

// When the ADK consumer stops early, the adapter must cancel the runtime it
// started and wait for the stream to close before returning, rather than
// leaving the model call running in the background.
func TestRunDockerAgent_EarlyStopCancelsAndDrainsRuntime(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		chunks []string
		stop   func(ctx *fakeInvocationContext) bool
	}{
		{
			name:   "consumer breaks",
			chunks: []string{"first"},
			stop:   func(*fakeInvocationContext) bool { return true },
		},
		{
			name:   "invocation ended",
			chunks: []string{"first", "second"},
			stop:   func(ctx *fakeInvocationContext) bool { ctx.EndInvocation(); return false },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			prov := &stallingProvider{chunks: tc.chunks}
			tm, root := newTeamWithProvider(prov)
			store := session.NewInMemorySessionStore()
			ctx := newFakeInvocationContext(t.Context(), "a2a-ctx-drain-"+tc.name, "Hi")

			var yielded int
			for _, err := range runDockerAgent(ctx, tm, root.Name(), root, store, servesafety.Resolved{Policy: session.SafetyPolicyRestricted}, testWorkspaceRoot) {
				require.NoError(t, err)
				yielded++
				if tc.stop(ctx) {
					break
				}
			}
			assert.Equal(t, 1, yielded)

			stream := prov.current()
			require.NotNil(t, stream, "the model must have been called")
			require.ErrorIs(t, stream.ctx.Err(), context.Canceled, "the adapter must cancel the runtime it abandons")
			assert.True(t, stream.closed.Load(), "the adapter must drain the runtime stream before returning")
		})
	}
}
