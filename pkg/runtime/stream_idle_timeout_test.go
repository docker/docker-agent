package runtime

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// configuredProvider exposes a full ModelConfig through BaseConfig so the
// runtime can read provider-specific options.
type configuredProvider struct {
	cfg    latest.ModelConfig
	stream chat.MessageStream
}

func (p *configuredProvider) ID() modelsdev.ID {
	return modelsdev.NewID(p.cfg.Provider, p.cfg.Model)
}

func (p *configuredProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return p.stream, nil
}

func (p *configuredProvider) BaseConfig() base.Config { return base.Config{ModelConfig: p.cfg} }
func (p *configuredProvider) MaxTokens() int          { return 0 }

func TestStreamIdleTimeoutFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider string
		opts     map[string]any
		want     time.Duration
	}{
		{name: "no opts", provider: "google", want: defaultStreamIdleTimeout},
		{name: "google flex", provider: "google", opts: map[string]any{"service_tier": "flex"}, want: flexStreamIdleTimeout},
		{name: "google priority", provider: "google", opts: map[string]any{"service_tier": "priority"}, want: defaultStreamIdleTimeout},
		{name: "google standard", provider: "google", opts: map[string]any{"service_tier": "standard"}, want: defaultStreamIdleTimeout},
		{name: "google non-string", provider: "google", opts: map[string]any{"service_tier": 1}, want: defaultStreamIdleTimeout},
		{name: "openai flex", provider: "openai", opts: map[string]any{"service_tier": "flex"}, want: defaultStreamIdleTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := &configuredProvider{cfg: latest.ModelConfig{Provider: tt.provider, Model: "m", ProviderOpts: tt.opts}}
			assert.Equal(t, tt.want, streamIdleTimeoutFor(p))
		})
	}
}

// Pins the idle boundary the fallback executor applies per provider: Gemini
// Flex may sit in Google's queue for up to 15 minutes before the first byte,
// while every other tier keeps the 5 minute default.
func TestFallbackExecutor_StreamIdleTimeoutByServiceTier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts map[string]any
		want time.Duration
	}{
		{name: "google flex", opts: map[string]any{"service_tier": "flex"}, want: flexStreamIdleTimeout},
		{name: "google priority", opts: map[string]any{"service_tier": "priority"}, want: defaultStreamIdleTimeout},
		{name: "google default tier", want: defaultStreamIdleTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				stream := newStalledStream()
				p := &configuredProvider{
					cfg:    latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash", ProviderOpts: tt.opts},
					stream: stream,
				}
				a := agent.New("root", "test", agent.WithModel(p))
				executor := newFallbackExecutor()
				executor.cooldowns = newCooldownManager(time.Now)

				errCh := make(chan error, 1)
				go func() {
					_, _, err := executor.execute(t.Context(), a, p, nil, nil, session.New(), &collectSink{}, nil, nil)
					errCh <- err
				}()

				<-stream.recvStarted
				time.Sleep(tt.want - time.Nanosecond) //nolint:forbidigo // Advances synthetic time to the boundary.
				select {
				case err := <-errCh:
					t.Fatalf("idle timeout fired before %v: %v", tt.want, err)
				default:
				}
				synctest.Sleep(time.Nanosecond) // Crosses the synthetic timeout boundary.
				select {
				case err := <-errCh:
					require.ErrorIs(t, err, errStreamIdle)
				default:
					t.Fatalf("idle timeout did not fire at %v", tt.want)
				}
			})
		})
	}
}
