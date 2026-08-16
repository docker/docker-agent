package runtime

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type imageOutputProvider struct {
	id     string
	stream chat.MessageStream
	calls  atomic.Int64
}

func (p *imageOutputProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(p.id) }

func (p *imageOutputProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	p.calls.Add(1)
	if p.stream == nil {
		return nil, errors.New("no stream configured")
	}
	return p.stream, nil
}

func (p *imageOutputProvider) BaseConfig() base.Config {
	return base.Config{ModelConfig: latest.ModelConfig{
		OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)},
	}}
}

func (p *imageOutputProvider) MaxTokens() int { return 0 }

func newTitleTestRuntime(t *testing.T, root *agent.Agent) *LocalRuntime {
	t.Helper()
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	return rt
}

// Image-output models are safe title candidates because Gemini title requests
// deliberately omit response modalities and the media marker instruction.
func TestLocalRuntime_TitleGenerator_AllImageOutputCandidatesRemainEligible(t *testing.T) {
	t.Parallel()

	primary := &imageOutputProvider{id: "google/image-primary"}
	title := &imageOutputProvider{id: "google/image-title"}
	fallback := &imageOutputProvider{id: "google/image-fallback"}
	root := agent.New("root", "test",
		agent.WithModel(primary),
		agent.WithTitleModel(title),
		agent.WithFallbackModel(fallback),
	)
	rt := newTitleTestRuntime(t, root)

	assert.NotNil(t, rt.TitleGenerator(t.Context()),
		"image-output models remain eligible for text-only title generation")
	assert.Zero(t, primary.calls.Load())
	assert.Zero(t, title.calls.Load())
	assert.Zero(t, fallback.calls.Load())
}

// A dedicated image-output title model remains first in the candidate order;
// title requests are text-only, and failure falls through to the agent model.
func TestLocalRuntime_TitleGenerator_ImageOutputTitleModelFallsThrough(t *testing.T) {
	t.Parallel()

	imageTitle := &imageOutputProvider{id: "google/image-title"}
	safe := &countingProvider{
		id:     "safe/model",
		stream: newStreamBuilder().AddContent("A Title").AddStopWithUsage(5, 3).Build(),
	}
	root := agent.New("root", "test",
		agent.WithModel(safe),
		agent.WithTitleModel(imageTitle),
	)
	rt := newTitleTestRuntime(t, root)

	gen := rt.TitleGenerator(t.Context())
	require.NotNil(t, gen)

	generated, err := gen.Generate(t.Context(), "sess-1", []string{"hello"})
	require.NoError(t, err)
	assert.Equal(t, "A Title", generated)
	assert.Equal(t, int64(1), imageTitle.calls.Load(), "the image-output title model is attempted first")
	assert.Equal(t, 1, safe.callCount)
}

// The title candidate list must not affect the main generation stream: an
// image-output model remains usable by both normal and text-only title calls.
func TestRunStream_ImageOutputOnlyModel_MainStreamUnaffected(t *testing.T) {
	t.Parallel()

	provider := &imageOutputProvider{
		id:     "google/image-model",
		stream: newStreamBuilder().AddContent("here is your cat").AddStopWithUsage(10, 5).Build(),
	}
	root := agent.New("root", "test", agent.WithModel(provider))
	rt := newTitleTestRuntime(t, root)

	require.NotNil(t, rt.TitleGenerator(t.Context()))

	sess := session.New(session.WithUserMessage("draw a cat"))
	var content strings.Builder
	for ev := range rt.RunStream(t.Context(), sess) {
		switch e := ev.(type) {
		case *ErrorEvent:
			t.Fatalf("main stream must stay successful, got ErrorEvent %q", e.Error)
		case *AgentChoiceEvent:
			content.WriteString(e.Content)
		}
	}

	assert.Equal(t, "here is your cat", content.String())
	assert.Equal(t, int64(1), provider.calls.Load(), "exactly the main generation request reaches the provider")
	assert.Empty(t, sess.Title, "the session keeps its default title when title generation is skipped")
}
