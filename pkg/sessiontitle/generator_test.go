package sessiontitle

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

type mockProvider struct {
	id        modelsdev.ID
	calls     int
	createFn  func() (chat.MessageStream, error)
	baseCfgFn func() base.Config
}

func (p *mockProvider) ID() modelsdev.ID { return p.id }

func (p *mockProvider) CreateChatCompletionStream(
	_ context.Context,
	_ []chat.Message,
	_ []tools.Tool,
) (chat.MessageStream, error) {
	p.calls++
	return p.createFn()
}

func (p *mockProvider) BaseConfig() base.Config {
	if p.baseCfgFn != nil {
		return p.baseCfgFn()
	}
	return base.Config{}
}

type mockStream struct {
	responses []chat.MessageStreamResponse
	i         int

	errAt int
	err   error
}

func (s *mockStream) Recv() (chat.MessageStreamResponse, error) {
	if s.errAt >= 0 && s.i == s.errAt {
		return chat.MessageStreamResponse{}, s.err
	}
	if s.i >= len(s.responses) {
		return chat.MessageStreamResponse{}, io.EOF
	}
	r := s.responses[s.i]
	s.i++
	return r, nil
}

func (s *mockStream) Close() {}

func streamWithContent(content string) chat.MessageStream {
	return &mockStream{
		responses: []chat.MessageStreamResponse{
			{
				Choices: []chat.MessageStreamChoice{
					{Delta: chat.MessageDelta{Content: content}},
				},
			},
		},
		errAt: -1,
	}
}

func TestGenerator_Generate_FallsBackOnStreamCreateError(t *testing.T) {
	t.Parallel()

	primary := &mockProvider{
		id: modelsdev.NewID("primary", "fail"),
		createFn: func() (chat.MessageStream, error) {
			return nil, errors.New("primary boom")
		},
	}
	fallback := &mockProvider{
		id: modelsdev.NewID("fallback", "success"),
		createFn: func() (chat.MessageStream, error) {
			return streamWithContent("My Title"), nil
		},
	}

	gen := New(primary, fallback)
	title, err := gen.Generate(t.Context(), "sess-1", []string{"hello"})
	require.NoError(t, err)
	assert.Equal(t, "My Title", title)
	assert.Equal(t, 1, primary.calls)
	assert.Equal(t, 1, fallback.calls)
}

func TestGenerator_Generate_FallsBackOnRecvError(t *testing.T) {
	t.Parallel()

	primaryStream := &mockStream{
		responses: []chat.MessageStreamResponse{
			{
				Choices: []chat.MessageStreamChoice{
					{Delta: chat.MessageDelta{Content: "Partial"}},
				},
			},
		},
		errAt: 1,
		err:   errors.New("recv boom"),
	}

	primary := &mockProvider{
		id: modelsdev.NewID("primary", "recv-error"),
		createFn: func() (chat.MessageStream, error) {
			return primaryStream, nil
		},
	}
	fallback := &mockProvider{
		id: modelsdev.NewID("fallback", "success"),
		createFn: func() (chat.MessageStream, error) {
			return streamWithContent("Recovered Title"), nil
		},
	}

	gen := New(primary, fallback)
	title, err := gen.Generate(t.Context(), "sess-1", []string{"hello"})
	require.NoError(t, err)
	assert.Equal(t, "Recovered Title", title)
	assert.Equal(t, 1, primary.calls)
	assert.Equal(t, 1, fallback.calls)
}

func TestGenerator_Generate_FallsBackOnEmptyOutput(t *testing.T) {
	t.Parallel()

	primary := &mockProvider{
		id: modelsdev.NewID("primary", "empty"),
		createFn: func() (chat.MessageStream, error) {
			return streamWithContent("\n\n"), nil
		},
	}
	fallback := &mockProvider{
		id: modelsdev.NewID("fallback", "success"),
		createFn: func() (chat.MessageStream, error) {
			return streamWithContent("Good Title"), nil
		},
	}

	gen := New(primary, fallback)
	title, err := gen.Generate(t.Context(), "sess-1", []string{"hello"})
	require.NoError(t, err)
	assert.Equal(t, "Good Title", title)
	assert.Equal(t, 1, primary.calls)
	assert.Equal(t, 1, fallback.calls)
}
