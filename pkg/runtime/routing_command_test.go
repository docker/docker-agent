//go:build !windows

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamloader"
)

// The command-selector example is the offline fixture: no evaluator and no judge request.
func TestRoutingCommandExample(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile(filepath.Join("..", "..", "examples", "hook_routing_command.yaml"))
	require.NoError(t, err)

	for _, tt := range []struct {
		request     string
		wantAnswer  string
		wantRouting bool
	}{
		{"Diagnose this distributed DEADLOCK", "gpt-5 answer", true},
		{"What does EXPOSE do?", "gpt-5-mini answer", false},
	} {
		t.Run(tt.request, func(t *testing.T) {
			t.Parallel()
			providers := map[string]*scriptedProvider{}
			factory := func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (provider.Provider, error) {
				p := &scriptedProvider{reply: cfg.Model + " answer"}
				providers[cfg.Model] = p
				return p, nil
			}
			tm, err := teamloader.Load(t.Context(), config.NewBytesSource("hook_routing_command.yaml", source),
				&config.RuntimeConfig{EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "fake"})},
				teamloader.WithProviderRegistry(provider.NewRegistry(map[string]provider.Factory{"openai": factory})))
			require.NoError(t, err)
			rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, rt.Close()) })

			sess := session.New(session.WithUserMessage(tt.request), session.WithNonInteractive(true))
			events := runRouted(t, rt, sess)

			assert.Empty(t, routedErrors(events))
			assert.Equal(t, tt.wantRouting, len(routeEvents(events)) == 1)
			assert.Equal(t, tt.wantAnswer, sess.GetLastAssistantMessageContent())
			if tt.wantRouting {
				assert.Zero(t, providers["gpt-5-mini"].calls.Load(), "the routed-away agent never calls its model")
			}
		})
	}
}
