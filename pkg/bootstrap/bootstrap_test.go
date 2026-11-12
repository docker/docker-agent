package bootstrap

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

type testProvider struct {
	base.Config
}

func (testProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return &testStream{}, nil
}

func (testProvider) MaxTokens() int { return 0 }

type testStream struct {
	done bool
}

func (s *testStream) Recv() (chat.MessageStreamResponse, error) {
	if s.done {
		return chat.MessageStreamResponse{}, io.EOF
	}
	s.done = true
	return chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "hello"}}},
		Usage:   &chat.Usage{InputTokens: 3, OutputTokens: 2},
	}, nil
}

func (*testStream) Close() {}

func testLoaded() *teamloader.LoadResult {
	p := &testProvider{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "test", Model: "model"}}}
	return &teamloader.LoadResult{
		Team: team.New(team.WithAgents(agent.New("root", "", agent.WithModel(p)))),
	}
}

func TestRuntimeOptsModelConfiguration(t *testing.T) {
	t.Parallel()

	loaded := testLoaded()
	loaded.Models = map[string]latest.ModelConfig{"alternate": {Provider: "test", Model: "alternate"}}
	loaded.Providers = map[string]latest.ProviderConfig{"custom": {}}
	loaded.AgentDefaultModels = map[string]string{"root": "alternate"}
	loaded.EncryptedConfig = "loaded-encrypted-config"
	env := environment.NewMapEnvProvider(map[string]string{"TEST_KEY": "value"})
	store := modelsdev.NewDatabaseStore(&modelsdev.Database{})
	runConfig := &config.RuntimeConfig{
		Config:                 config.Config{ModelsGateway: "https://gateway.example", EncryptedConfig: "unresolved-config"},
		EnvProviderOverride:    env,
		ModelsDevStoreOverride: store,
	}
	calls := 0
	loaded.ProviderRegistry = provider.NewRegistry(map[string]provider.Factory{
		"test": func(_ context.Context, cfg *latest.ModelConfig, gotEnv environment.Provider, opts ...options.Opt) (provider.Provider, error) {
			calls++
			assert.Equal(t, loaded.Models["alternate"].Model, cfg.Model)
			assert.Same(t, env, gotEnv)
			gotOpts := options.Apply(opts...)
			assert.Equal(t, runConfig.ModelsGateway, gotOpts.Gateway())
			assert.Equal(t, loaded.EncryptedConfig, gotOpts.EncryptedConfig())
			assert.Equal(t, loaded.Providers, gotOpts.Providers())
			assert.Same(t, store, gotOpts.ModelsDevStore())
			return &testProvider{Config: base.Config{ModelConfig: *cfg}}, nil
		},
	})

	opts := RuntimeOpts(loaded, runConfig)
	assert.Zero(t, calls, "assembly must not construct model clients")
	rt, err := runtime.New(t.Context(), loaded.Team, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	assert.True(t, rt.SupportsModelSwitching())
	require.NoError(t, rt.SetAgentModel(t.Context(), "root", "alternate"))
	assert.Equal(t, 1, calls)
	require.NoError(t, rt.SetAgentModel(t.Context(), "root", ""))
	assert.Equal(t, 1, calls, "clearing an override must reuse the original model")
	ag, err := loaded.Team.DefaultAgent()
	require.NoError(t, err)
	assert.Equal(t, "test/model", ag.Model(t.Context()).ID().String())
}

func TestRuntimeOptsBudgets(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"run", "shared", "unbudgeted"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			loaded := testLoaded()
			switch name {
			case "run":
				loaded.Budget = &latest.BudgetConfig{MaxTokens: 10}
			case "shared":
				loaded.Budgets = map[string]latest.BudgetConfig{"shared": {MaxTokens: 10}, "unused": {MaxTokens: 1}}
				loaded.AgentBudgets = map[string][]string{"root": {"shared"}}
			}
			runConfig := &config.RuntimeConfig{
				EnvProviderOverride:    environment.NewMapEnvProvider(nil),
				ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{}),
			}
			rt, err := runtime.New(t.Context(), loaded.Team, RuntimeOpts(loaded, runConfig)...)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close()) })

			sess := session.New(session.WithUserMessage("hello"))
			var statuses []runtime.BudgetStatus
			for event := range rt.RunStream(t.Context(), sess) {
				if usage, ok := event.(*runtime.BudgetUsageEvent); ok {
					statuses = usage.Budgets
				}
			}
			assert.Equal(t, "hello", sess.GetLastAssistantMessageContent())
			if name == "unbudgeted" {
				assert.Empty(t, statuses)
				return
			}
			require.Len(t, statuses, 1)
			assert.Equal(t, name, statuses[0].Name)
			assert.Equal(t, int64(10), statuses[0].MaxTokens)
			assert.Equal(t, int64(5), statuses[0].Tokens)
		})
	}
}

func TestRuntimeOptsCallerOverrides(t *testing.T) {
	t.Parallel()
	loaded := testLoaded()
	loaded.Models = map[string]latest.ModelConfig{"alternate": {Provider: "test", Model: "alternate"}}
	overrideStore := modelsdev.NewDatabaseStore(&modelsdev.Database{})
	called := false
	loaded.ProviderRegistry = provider.NewRegistry(map[string]provider.Factory{
		"test": func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, opts ...options.Opt) (provider.Provider, error) {
			called = true
			gotOpts := options.Apply(opts...)
			assert.Same(t, overrideStore, gotOpts.ModelsDevStore())
			return &testProvider{Config: base.Config{ModelConfig: *cfg}}, nil
		},
	})
	runConfig := &config.RuntimeConfig{
		EnvProviderOverride:    environment.NewMapEnvProvider(nil),
		ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{}),
	}
	opts := RuntimeOpts(loaded, runConfig)
	store := session.NewInMemorySessionStore()
	opts = append(opts, runtime.WithModelStore(overrideStore), runtime.WithSessionStore(store))
	rt, err := runtime.New(t.Context(), loaded.Team, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	assert.Same(t, store, rt.SessionStore())
	require.NoError(t, rt.SetAgentModel(t.Context(), "root", "alternate"))
	assert.True(t, called)
}

func TestRuntimeOptsResolvesEnvironmentWhenAssembled(t *testing.T) {
	t.Parallel()
	workingDir := t.TempDir()
	file := filepath.Join(workingDir, "test.env")
	require.NoError(t, os.WriteFile(file, []byte("TEST_KEY=before\n"), 0o600))
	runConfig := &config.RuntimeConfig{
		Config:                 config.Config{WorkingDir: workingDir, EnvFiles: []string{"test.env"}},
		ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{}),
	}
	loaded := testLoaded()
	opts := RuntimeOpts(loaded, runConfig)
	require.NoError(t, os.WriteFile(file, []byte("TEST_KEY=after\n"), 0o600))
	rt, err := runtime.New(t.Context(), loaded.Team, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	value, ok := runConfig.EnvProvider().Get(t.Context(), "TEST_KEY")
	require.True(t, ok)
	assert.Equal(t, "before", value)
}

func TestRuntimeOptsModelStoreFailureFallsBack(t *testing.T) {
	// Cache-dir overrides are process-global.
	original := paths.GetCacheDir()
	paths.SetCacheDir(filepath.Join(t.TempDir(), "file"))
	t.Cleanup(func() { paths.SetCacheDir(original) })
	require.NoError(t, os.WriteFile(paths.GetCacheDir(), nil, 0o600))
	loaded := testLoaded()
	runConfig := &config.RuntimeConfig{EnvProviderOverride: environment.NewMapEnvProvider(nil)}
	opts := RuntimeOpts(loaded, runConfig)
	_, err := runConfig.ModelsDevStore()
	require.Error(t, err)
	rt, err := runtime.New(t.Context(), loaded.Team, opts...)
	require.NoError(t, err, "failed catalog initialization must not prevent runtime construction")
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
}
