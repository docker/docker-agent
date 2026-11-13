package root

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/contracts"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// providerNameStub reports a provider name without opening a model client.
type providerNameStub struct{ name string }

func (p providerNameStub) ID() modelsdev.ID { return modelsdev.NewID(p.name, "m") }

func (providerNameStub) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, nil
}

func (providerNameStub) BaseConfig() contracts.Config { return contracts.Config{} }

func TestNewSessionMaxIterations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		explicit  int
		providers []string
		want      int
	}{
		{name: "omitted flag on dmr", explicit: 0, providers: []string{"dmr"}, want: 20},
		{name: "omitted flag on DMR", explicit: 0, providers: []string{"DMR"}, want: 20},
		{name: "omitted flag on openai", explicit: 0, providers: []string{"openai"}, want: 0},
		{name: "omitted flag with no model", explicit: 0, want: 0},
		{name: "explicit limit on dmr", explicit: 7, providers: []string{"dmr"}, want: 7},
		{name: "explicit limit on openai", explicit: 4, providers: []string{"openai"}, want: 4},
		{name: "dmr among several models", explicit: 0, providers: []string{"openai", "dmr"}, want: 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := make([]agent.Opt, 0, len(tt.providers))
			for _, name := range tt.providers {
				opts = append(opts, agent.WithModel(providerNameStub{name: name}))
			}
			tm := team.New(team.WithAgents(agent.New("root", "", opts...)))

			got := newSessionMaxIterations(tt.explicit, tm)
			assert.Equal(t, tt.want, got)

			sess := session.New(session.WithMaxIterations(got))
			assert.Equal(t, tt.want, sess.MaxIterations, "the session stores the value passed to WithMaxIterations; 0 is unlimited")
		})
	}

	t.Run("nil team", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 0, newSessionMaxIterations(0, nil))
		assert.Equal(t, 3, newSessionMaxIterations(3, nil))
	})
}
