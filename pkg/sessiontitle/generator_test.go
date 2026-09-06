package sessiontitle

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
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

func TestGenerator_Generate_AllModelsFail_ReportsEachModel(t *testing.T) {
	t.Parallel()

	primary := &mockProvider{
		id: modelsdev.NewID("primary", "boom"),
		createFn: func() (chat.MessageStream, error) {
			return nil, errors.New("exit status 1")
		},
	}
	fallback := &mockProvider{
		id: modelsdev.NewID("fallback", "nope"),
		createFn: func() (chat.MessageStream, error) {
			return nil, errors.New("connection refused")
		},
	}

	gen := New(primary, fallback)
	title, err := gen.Generate(t.Context(), "sess-1", []string{"hello"})
	require.Error(t, err)
	assert.Empty(t, title)

	// Every attempt is surfaced, each tagged with the model that failed,
	// so a bare leaf like "exit status 1" is no longer the whole message.
	msg := err.Error()
	assert.Contains(t, msg, "all 2 title model(s) failed")
	assert.Contains(t, msg, `model "primary/boom"`)
	assert.Contains(t, msg, "exit status 1")
	assert.Contains(t, msg, `model "fallback/nope"`)
	assert.Contains(t, msg, "connection refused")
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

// imageOutputProvider is an image-capable title candidate.
func imageOutputProvider(id modelsdev.ID) *mockProvider {
	return &mockProvider{
		id: id,
		createFn: func() (chat.MessageStream, error) {
			return nil, errors.New("image-output title attempt failed")
		},
		baseCfgFn: func() base.Config {
			return base.Config{ModelConfig: latest.ModelConfig{
				OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)},
			}}
		},
	}
}

func TestNew_ImageOutputCandidateFallsThroughOnFailure(t *testing.T) {
	t.Parallel()

	img := imageOutputProvider(modelsdev.NewID("google", "image-primary"))
	safe := &mockProvider{
		id: modelsdev.NewID("safe", "fallback"),
		createFn: func() (chat.MessageStream, error) {
			return streamWithContent("Safe Title"), nil
		},
	}

	title, err := New(img, safe).Generate(t.Context(), "sess-1", []string{"hello"})
	require.NoError(t, err)
	assert.Equal(t, "Safe Title", title)
	assert.Equal(t, 1, img.calls)
	assert.Equal(t, 1, safe.calls)
}

func TestNew_AllImageOutputCandidatesRemainEligible(t *testing.T) {
	t.Parallel()

	img1 := imageOutputProvider(modelsdev.NewID("google", "image-one"))
	img2 := &mockProvider{
		id: modelsdev.NewID("google", "image-two"),
		createFn: func() (chat.MessageStream, error) {
			return streamWithContent("Image Model Title"), nil
		},
	}

	gen := New(img1, img2)
	require.NotNil(t, gen)
	title, err := gen.Generate(t.Context(), "sess-1", []string{"hello"})
	require.NoError(t, err)
	assert.Equal(t, "Image Model Title", title)
	assert.Equal(t, 1, img1.calls)
	assert.Equal(t, 1, img2.calls)
}

func TestNew_NoUsableCandidates_ReturnsNil(t *testing.T) {
	t.Parallel()

	assert.Nil(t, New())
	assert.Nil(t, New(nil, nil))
}

func TestNew_KeepsUndeclaredAndExplicitFalseCandidates(t *testing.T) {
	t.Parallel()

	undeclared := &mockProvider{
		id: modelsdev.NewID("safe", "undeclared"),
		createFn: func() (chat.MessageStream, error) {
			return streamWithContent("Ordinary Title"), nil
		},
	}
	declaredFalse := &mockProvider{
		id: modelsdev.NewID("safe", "declared-false"),
		createFn: func() (chat.MessageStream, error) {
			return streamWithContent("Fallback Title"), nil
		},
		baseCfgFn: func() base.Config {
			return base.Config{ModelConfig: latest.ModelConfig{
				OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(false)},
			}}
		},
	}

	gen := New(undeclared, declaredFalse)
	require.NotNil(t, gen)

	title, err := gen.Generate(t.Context(), "sess-1", []string{"hello"})
	require.NoError(t, err)
	assert.Equal(t, "Ordinary Title", title)
	assert.Equal(t, 1, undeclared.calls)
	assert.Zero(t, declaredFalse.calls)
}
