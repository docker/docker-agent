package runtime

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/contracts"
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
		{name: "no opts", provider: "google", want: latest.DisabledStreamIdleTimeout},
		{name: "google flex", provider: "google", opts: map[string]any{"service_tier": "flex"}, want: latest.DisabledStreamIdleTimeout},
		{name: "google priority", provider: "google", opts: map[string]any{"service_tier": "priority"}, want: latest.DisabledStreamIdleTimeout},
		{name: "google standard", provider: "google", opts: map[string]any{"service_tier": "standard"}, want: latest.DisabledStreamIdleTimeout},
		{name: "google non-string", provider: "google", opts: map[string]any{"service_tier": 1}, want: latest.DisabledStreamIdleTimeout},
		{name: "openai flex", provider: "openai", opts: map[string]any{"service_tier": "flex"}, want: latest.DisabledStreamIdleTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := &configuredProvider{cfg: latest.ModelConfig{Provider: tt.provider, Model: "m", ProviderOpts: tt.opts}}
			assert.Equal(t, tt.want, streamIdleTimeoutFor(p, nil, agent.New("root", "")))
		})
	}
}

func TestFallbackExecutor_UnconfiguredStreamRemainsCancelable(t *testing.T) {
	t.Parallel()

	flex := latest.ModelConfig{Provider: "google", Model: "gemini", ProviderOpts: map[string]any{"service_tier": "flex"}}
	for _, tc := range []struct {
		name     string
		entry    latest.ModelConfig
		target   *latest.ModelConfig
		fallback bool
	}{
		{name: "ordinary", entry: latest.ModelConfig{Provider: "openai", Model: "m"}},
		{name: "flex", entry: flex},
		{name: "flex entry routed to ordinary", entry: flex, target: &latest.ModelConfig{Provider: "openai"}},
		{name: "ordinary entry routed to flex", entry: latest.ModelConfig{Provider: "rulebased", Model: "router"}, target: &flex},
		{name: "fallback attempt", entry: flex, fallback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				stream := newStalledStream()
				var modelStream chat.MessageStream = stream
				if tc.target != nil {
					modelStream = &dispatchedIdleStream{MessageStream: stream, model: contracts.ServingModel{Config: base.Config{ModelConfig: *tc.target}}}
				}
				p := &configuredProvider{cfg: tc.entry, stream: modelStream}
				var primary provider.Provider = p
				var opts []agent.Opt
				if tc.fallback {
					primary = &failingProvider{id: "openai/primary", err: errors.New("failure")}
					opts = append(opts, agent.WithFallbackModel(p))
				}
				opts = append(opts, agent.WithModel(primary))
				a := agent.New("root", "test", opts...)
				executor := newFallbackExecutor()
				executor.cooldowns = newCooldownManager(time.Now)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				errCh := make(chan error, 1)
				go func() {
					_, _, err := executor.execute(ctx, a, primary, nil, nil, session.New(), &collectSink{}, nil, nil)
					errCh <- err
				}()

				<-stream.recvStarted
				synctest.Sleep(16 * time.Minute)
				select {
				case err := <-errCh:
					t.Fatalf("unconfigured stream returned before cancellation: %v", err)
				default:
				}
				cancel()
				synctest.Wait()
				select {
				case err := <-errCh:
					require.ErrorIs(t, err, context.Canceled)
				default:
					t.Fatal("stream did not promptly return after cancellation")
				}
			})
		})
	}
}

type dispatchedIdleStream struct {
	chat.MessageStream

	model contracts.ServingModel
}

func (s *dispatchedIdleStream) ServingModel() contracts.ServingModel { return s.model }

func TestStreamIdleTimeoutPrecedence(t *testing.T) {
	t.Parallel()
	timeout := func(d time.Duration) *latest.StreamIdleTimeout { return &latest.StreamIdleTimeout{Duration: d} }
	flex := latest.ModelConfig{Provider: "google", ProviderOpts: map[string]any{"service_tier": "flex"}}
	for _, tc := range []struct {
		name        string
		model, file *latest.StreamIdleTimeout
		entry       *latest.ModelConfig
		serving     *latest.ModelConfig
		want        time.Duration
	}{
		{name: "flex default", want: latest.DisabledStreamIdleTimeout},
		{name: "file overrides flex", file: timeout(time.Minute), want: time.Minute},
		{name: "model overrides file", model: timeout(2 * time.Minute), file: timeout(time.Minute), want: 2 * time.Minute},
		{name: "positive model overrides disabled file", model: timeout(2 * time.Minute), file: timeout(latest.DisabledStreamIdleTimeout), want: 2 * time.Minute},
		{name: "positive target overrides disabled entry and file", model: timeout(latest.DisabledStreamIdleTimeout), file: timeout(latest.DisabledStreamIdleTimeout), serving: &latest.ModelConfig{StreamIdleTimeout: timeout(3 * time.Minute)}, want: 3 * time.Minute},
		{name: "model disables", model: timeout(latest.DisabledStreamIdleTimeout), file: timeout(time.Minute), want: latest.DisabledStreamIdleTimeout},
		{name: "file disables", file: timeout(latest.DisabledStreamIdleTimeout), want: latest.DisabledStreamIdleTimeout},
		{name: "routed target wins", model: timeout(time.Second), file: timeout(time.Minute), serving: &latest.ModelConfig{Provider: "openai", StreamIdleTimeout: timeout(3 * time.Minute)}, want: 3 * time.Minute},
		{name: "routed target disables", model: timeout(time.Second), serving: &latest.ModelConfig{StreamIdleTimeout: timeout(latest.DisabledStreamIdleTimeout)}, want: latest.DisabledStreamIdleTimeout},
		{name: "entry wins over unconfigured target", model: timeout(2 * time.Second), file: timeout(time.Minute), serving: &latest.ModelConfig{Provider: "openai"}, want: 2 * time.Second},
		{name: "entry disables for target", model: timeout(latest.DisabledStreamIdleTimeout), serving: &latest.ModelConfig{Provider: "openai"}, want: latest.DisabledStreamIdleTimeout},
		{name: "file wins over unconfigured target", file: timeout(time.Minute), serving: &latest.ModelConfig{Provider: "openai"}, want: time.Minute},
		{name: "flex entry with non-flex target", serving: &latest.ModelConfig{Provider: "openai"}, want: latest.DisabledStreamIdleTimeout},
		{name: "non-flex entry with flex target", entry: &latest.ModelConfig{Provider: "rulebased"}, serving: &flex, want: latest.DisabledStreamIdleTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := flex
			if tc.entry != nil {
				cfg = *tc.entry
			}
			cfg.StreamIdleTimeout = tc.model
			p := &configuredProvider{cfg: cfg}
			var stream chat.MessageStream
			if tc.serving != nil {
				stream = &dispatchedIdleStream{model: contracts.ServingModel{Config: base.Config{ModelConfig: *tc.serving}}}
			}
			a := agent.New("root", "", agent.WithStreamIdleTimeout(tc.file))
			assert.Equal(t, tc.want, streamIdleTimeoutFor(p, stream, a))
		})
	}
}

func TestDisabledStreamTimeoutStillCancels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stream := newStalledStream()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := handleStream(ctx, nil, stream, agent.New("root", ""), nil, session.New(), &collectSink{}, latest.DisabledStreamIdleTimeout)
			result <- err
		}()
		<-stream.recvStarted
		synctest.Sleep(24 * time.Hour)
		select {
		case err := <-result:
			t.Fatalf("disabled timer returned: %v", err)
		default:
		}
		cancel()
		require.ErrorIs(t, <-result, context.Canceled)
	})
}

func TestFallbackUsesItsOwnStreamTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		primary := &failingProvider{id: "openai/primary", err: errors.New("failure")}
		stream := newStalledStream()
		fallback := &configuredProvider{cfg: latest.ModelConfig{Provider: "openai", Model: "fallback", StreamIdleTimeout: &latest.StreamIdleTimeout{Duration: 2 * time.Second}}, stream: &dispatchedIdleStream{MessageStream: stream, model: contracts.ServingModel{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "google"}}}}}
		a := agent.New("root", "", agent.WithModel(primary), agent.WithFallbackModel(fallback), agent.WithStreamIdleTimeout(&latest.StreamIdleTimeout{Duration: 10 * time.Second}))
		e := newFallbackExecutor()
		e.cooldowns = newCooldownManager(time.Now)
		result := make(chan error, 1)
		go func() {
			_, _, err := e.execute(t.Context(), a, primary, nil, nil, session.New(), &collectSink{}, nil, nil)
			result <- err
		}()
		<-stream.recvStarted
		synctest.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case err := <-result:
			require.ErrorIs(t, err, errStreamIdle)
		default:
			t.Fatal("fallback did not time out at its configured deadline")
		}
	})
}

func TestRoutedRetryRecomputesStreamTimeout(t *testing.T) {
	for _, ordinary := range []bool{false, true} {
		t.Run(map[bool]string{false: "immediate idle retry", true: "ordinary retry"}[ordinary], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				first := newStalledStream()
				second := newStalledStream()
				target := func(stream chat.MessageStream, timeout time.Duration) chat.MessageStream {
					return &dispatchedIdleStream{MessageStream: stream, model: contracts.ServingModel{Config: base.Config{ModelConfig: latest.ModelConfig{StreamIdleTimeout: &latest.StreamIdleTimeout{Duration: timeout}}}}}
				}
				firstStream := target(first, time.Second)
				retries := -1
				allowance := &idleStreamRetryAllowance{remaining: true, parentSessionID: "parent"}
				if ordinary {
					firstStream = target(&pricingErrorStream{err: errors.New("500 internal server error")}, time.Second)
					retries = 1
					allowance = nil
				}
				p := &idleThenResultProvider{id: "test/router", streams: []chat.MessageStream{firstStream, target(second, 3*time.Second)}}
				a := agent.New("delegate", "", agent.WithModel(p), agent.WithFallbackRetries(retries))
				e := newFallbackExecutor()
				e.cooldowns = newCooldownManager(time.Now)
				result := make(chan error, 1)
				go func() {
					_, _, err := e.execute(t.Context(), a, p, nil, nil, session.New(), &collectSink{}, allowance, func() error { return nil })
					result <- err
				}()
				if !ordinary {
					<-first.recvStarted
					synctest.Sleep(time.Second)
				}
				<-second.recvStarted
				synctest.Sleep(3*time.Second - time.Nanosecond)
				select {
				case err := <-result:
					t.Fatalf("retry timed out early: %v", err)
				default:
				}
				synctest.Sleep(time.Nanosecond)
				synctest.Wait()
				select {
				case err := <-result:
					require.ErrorIs(t, err, errStreamIdle)
				default:
					t.Fatal("retry did not time out at the routed target's deadline")
				}
				assert.Equal(t, 2, p.callCount())
			})
		})
	}
}
