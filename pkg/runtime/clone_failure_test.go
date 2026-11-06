package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type cloneFailureProvider struct {
	base.Config

	endpoint string
}

func (p *cloneFailureProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, http.NoBody)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	res.Body.Close()
	return &mockStream{}, nil
}

func (p *cloneFailureProvider) ContextWindow(ctx context.Context) (int64, error) {
	_, err := p.CreateChatCompletionStream(ctx, nil, nil)
	return 16_000, err
}

func TestCloneFailuresReachRuntimeCallers(t *testing.T) {
	t.Parallel()
	synthetic := errors.New("synthetic reconstruction failure")
	for _, tt := range []struct {
		name    string
		rebuild base.RebuildProviderFunc
		want    error
	}{
		{name: "missing rebuilder", want: provider.ErrNoRebuilder},
		{name: "reconstruction failure", want: synthetic, rebuild: func(context.Context, *latest.ModelConfig, ...options.Opt) (provider.Provider, error) {
			return nil, synthetic
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusOK) }))
			defer server.Close()
			p := &cloneFailureProvider{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "test", Model: "base", ProviderOpts: map[string]any{"context_size": 100_000}}, RebuildProvider: tt.rebuild}, endpoint: server.URL}
			a := agent.New("root", "test", agent.WithModel(p), agent.WithCompactionModel(p))
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithCurrentAgent("root"), WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
			require.NoError(t, err)
			n, err := rt.compactionContextLimitChecked(t.Context(), a)
			assert.Zero(t, n)
			require.ErrorIs(t, err, tt.want)
			assert.Zero(t, rt.compactionContextLimit(t.Context(), a))
			assert.Equal(t, int64(100_000), rt.effectiveContextLimit(t.Context(), a, 100_000))
			sess := session.New(session.WithMessages([]session.Item{session.NewMessageItem(session.UserMessage("keep this conversation"))}))
			before := sess.MessagesSnapshot()
			events := make(chan Event, 32)
			rt.Summarize(t.Context(), sess, "", NewChannelSink(events))
			close(events)
			var failures int
			for event := range events {
				switch e := event.(type) {
				case *ErrorEvent:
					failures++
					assert.Contains(t, e.Error, "resolving compaction context window")
					assert.Contains(t, e.Error, tt.want.Error())
				case *SessionCompactionEvent:
					if e.Status == "completed" {
						assert.Equal(t, CompactionOutcomeFailed, e.Outcome)
					}
				case *SessionSummaryEvent:
					t.Error("unexpected summary")
				}
			}
			assert.Equal(t, 1, failures)
			assert.Equal(t, before, sess.MessagesSnapshot())
			assert.Zero(t, requests.Load(), "neither inference nor base discovery is allowed")
		})
	}
}
