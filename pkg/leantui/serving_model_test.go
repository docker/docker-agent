package leantui

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/rulebased"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type footerProvider struct {
	base.Config

	err error
}

func (p *footerProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	if p.err != nil {
		return nil, p.err
	}
	return &footerStream{}, nil
}

type footerStream struct{ done bool }

func (s *footerStream) Recv() (chat.MessageStreamResponse, error) {
	if s.done {
		return chat.MessageStreamResponse{}, io.EOF
	}
	s.done = true
	return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "ok"}, FinishReason: chat.FinishReasonStop}}, Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1}}, nil
}
func (*footerStream) Close() {}

func TestServingModelFallbackFooter(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, prompt, want string
		router             bool
	}{
		{name: "direct", prompt: "hi", want: "test/fallback"},
		{name: "router no match", router: true, prompt: "hi", want: "test/fallback"},
		{name: "router matched", router: true, prompt: "poetry", want: "test/primary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			primary := &footerProvider{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "test", Model: "primary"}}, err: errors.New("401 unauthorized")}
			fallback := rulebased.Provider(&footerProvider{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "test", Model: "fallback", Cost: &latest.CostConfig{Input: 1}}}})
			if tc.router {
				cfg := fallback.BaseConfig().ModelConfig
				cfg.Routing = []latest.RoutingRule{{Model: "test/primary", Examples: []string{"poetry"}}}
				var err error
				fallback, err = rulebased.NewClient(t.Context(), &cfg, nil, nil, func(_ context.Context, spec string, _ map[string]latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (rulebased.Provider, error) {
					id := modelsdev.ParseIDOrZero(spec)
					return &footerProvider{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: id.Provider, Model: id.Model, Cost: &latest.CostConfig{Input: 1}}}}, nil
				})
				require.NoError(t, err)
			}
			root := agent.New("root", "test", agent.WithModel(primary), agent.WithFallbackModel(fallback), agent.WithFallbackRetries(-1))
			rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), runtime.WithModelStore(modelsdev.NewDatabaseStore(&modelsdev.Database{})), runtime.WithSessionCompaction(false))
			require.NoError(t, err)
			sess := session.New(session.WithTitle("Serving footer test"), session.WithUserMessage(tc.prompt))
			m := bareModel(24)
			for range 2 {
				for event := range rt.RunStream(t.Context(), sess) {
					switch event := event.(type) {
					case *runtime.ErrorEvent:
						t.Errorf("runtime error: %s", event.Error)
					case *runtime.AgentInfoEvent, *runtime.ModelFallbackEvent:
						m.handleEvent(t.Context(), event)
					}
				}
				assert.Equal(t, tc.want, m.status.Model)
			}
		})
	}
}
