package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

type configurationProvider struct{ base.Config }

func (*configurationProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, errors.New("configuration must not call model")
}

func newConfigurationAgent(t *testing.T, store session.Store) (*Agent, *atomic.Int32) {
	t.Helper()
	a := NewAgent(config.NewBytesSource("agent.yaml", nil), &config.RuntimeConfig{EnvProviderOverride: environment.NewMapEnvProvider(nil)}, store)
	var creations atomic.Int32
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		budget := &latest.ThinkingBudget{Effort: "adaptive"}
		models := map[string]latest.ModelConfig{
			"default":     {Name: "default", Provider: "openai", Model: "gpt-5", MaxTokens: new(int64(100))},
			"alternative": {Name: "alternative", Provider: "anthropic", Model: "claude-sonnet-4-0", MaxTokens: new(int64(100)), ThinkingBudget: budget},
		}
		factory := func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (provider.Provider, error) {
			creations.Add(1)
			return &configurationProvider{Config: base.Config{ModelConfig: *cfg.Clone()}}, nil
		}
		registry := provider.NewRegistry(map[string]provider.Factory{"openai": factory, "anthropic": factory})
		root := agent.New("root", "test", agent.WithModel(&configurationProvider{Config: base.Config{ModelConfig: models["default"]}}), agent.WithCommands(types.Commands{"worker": {Agent: "worker"}}))
		worker := agent.New("worker", "test", agent.WithModel(&configurationProvider{Config: base.Config{ModelConfig: models["alternative"]}}))
		return &teamloader.LoadResult{Team: team.New(team.WithAgents(root, worker)), Models: models, AgentDefaultModels: map[string]string{"root": "default", "worker": "alternative"}, ProviderRegistry: registry}, nil
	}
	_, err := a.Initialize(t.Context(), acpsdk.InitializeRequest{})
	require.NoError(t, err)
	captureReplay(t, a, &captureWriter{})
	return a, &creations
}

func configValue(t *testing.T, configOptions []acpsdk.SessionConfigOption, id string) string {
	t.Helper()
	for _, option := range configOptions {
		if option.Select != nil && string(option.Select.Id) == id {
			return string(option.Select.CurrentValue)
		}
	}
	t.Fatalf("option %s absent", id)
	return ""
}

func configRequest(sid acpsdk.SessionId, id, value string) acpsdk.SetSessionConfigOptionRequest {
	return acpsdk.SetSessionConfigOptionRequest{ValueId: &acpsdk.SetSessionConfigOptionValueId{SessionId: sid, ConfigId: acpsdk.SessionConfigId(id), Value: acpsdk.SessionConfigValueId(value)}}
}

func TestSessionConfigurationPersistenceAndReasoning(t *testing.T) {
	t.Parallel()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	a, creations := newConfigurationAgent(t, store)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	wd := t.TempDir()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd})
	require.NoError(t, err)
	assert.Equal(t, "default", configValue(t, created.ConfigOptions, "mode"))
	assert.Equal(t, "default", configValue(t, created.ConfigOptions, "model"))
	assert.Equal(t, "default", configValue(t, created.ConfigOptions, "thought_level"))
	assert.Equal(t, acpsdk.SessionModeId("default"), created.Modes.CurrentModeId)
	assert.Zero(t, creations.Load(), "option rendering must not create providers")
	s := a.sessions[string(created.SessionId)]
	response, err := a.SetSessionConfigOption(t.Context(), configRequest(created.SessionId, "model", "model:alternative"))
	require.NoError(t, err)
	assert.Equal(t, "model:alternative", configValue(t, response.ConfigOptions, "model"))
	assert.Equal(t, "default", configValue(t, response.ConfigOptions, "thought_level"), "adaptive is not none")
	response, err = a.SetSessionConfigOption(t.Context(), configRequest(created.SessionId, "thought_level", "high"))
	require.NoError(t, err)
	assert.Equal(t, "high", configValue(t, response.ConfigOptions, "thought_level"))
	response, err = a.SetSessionConfigOption(t.Context(), configRequest(created.SessionId, "mode", "strict"))
	require.NoError(t, err)
	assert.Equal(t, "strict", configValue(t, response.ConfigOptions, "mode"))
	resumed, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd})
	require.NoError(t, err)
	assert.Equal(t, "high", configValue(t, resumed.ConfigOptions, "thought_level"), "active resume retains runtime-only reasoning")
	_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	loaded, err := a.LoadSession(t.Context(), loadRequest(s))
	require.NoError(t, err)
	assert.Equal(t, "model:alternative", configValue(t, loaded.ConfigOptions, "model"))
	assert.Equal(t, "default", configValue(t, loaded.ConfigOptions, "thought_level"), "cold load restores configured adaptive budget")
	assert.Equal(t, "strict", configValue(t, loaded.ConfigOptions, "mode"))
	second, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	assert.Equal(t, "default", configValue(t, second.ConfigOptions, "model"))
	assert.Equal(t, "default", configValue(t, second.ConfigOptions, "mode"))
	_, err = a.SetSessionConfigOption(t.Context(), configRequest(second.SessionId, "thought_level", "high"))
	require.NoError(t, err)
	current := a.sessions[string(second.SessionId)].configuration(t.Context())
	assert.Equal(t, "default", configValue(t, current.Options, "model"), "reasoning overrides preserve the default model selection")
	response, err = a.SetSessionConfigOption(t.Context(), configRequest(second.SessionId, "thought_level", "default"))
	require.NoError(t, err)
	assert.Equal(t, "default", configValue(t, response.ConfigOptions, "thought_level"))
}

type failingConfigStore struct{ session.Store }

func (failingConfigStore) UpdateSession(context.Context, *session.Session) error {
	return errors.New("persist failed")
}

func TestSessionConfigurationRollbackAndSafety(t *testing.T) {
	t.Parallel()
	a, _ := newConfigurationAgent(t, session.NewInMemorySessionStore())
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	root, err := s.team.Agent("root")
	require.NoError(t, err)
	_, err = a.SetSessionConfigOption(t.Context(), configRequest(created.SessionId, "thought_level", "high"))
	require.NoError(t, err)
	before := root.EffectiveModels()
	s.sess.SetPermissions(&session.PermissionsConfig{Allow: []string{"one_tool"}, Deny: []string{"blocked"}})
	_, err = a.SetSessionMode(t.Context(), acpsdk.SetSessionModeRequest{SessionId: created.SessionId, ModeId: "autonomous"})
	require.NoError(t, err)
	store := a.sessionStore
	a.sessionStore = failingConfigStore{store}
	_, err = a.SetSessionConfigOption(t.Context(), configRequest(created.SessionId, "model", "model:alternative"))
	require.ErrorContains(t, err, "persist failed")
	assert.Equal(t, before, root.EffectiveModels())
	assert.Empty(t, s.sess.AgentModelOverrides)
	_, err = a.SetSessionMode(t.Context(), acpsdk.SetSessionModeRequest{SessionId: created.SessionId, ModeId: "strict"})
	require.ErrorContains(t, err, "persist failed")
	assert.Equal(t, session.SafetyPolicyAutonomous, s.sess.GetSafetyPolicy())
	a.sessionStore = store
	_, err = a.SetSessionMode(t.Context(), acpsdk.SetSessionModeRequest{SessionId: created.SessionId, ModeId: "default"})
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicy(""), s.sess.GetSafetyPolicy())
	assert.False(t, s.sess.IsToolsApproved())
	assert.Equal(t, &session.PermissionsConfig{Allow: []string{"one_tool"}, Deny: []string{"blocked"}}, s.sess.ClonePermissions())
}

func TestSessionConfigurationRejectsInvalidAndBusyRequests(t *testing.T) {
	t.Parallel()
	a, creations := newConfigurationAgent(t, session.NewInMemorySessionStore())
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	for _, request := range []acpsdk.SetSessionConfigOptionRequest{
		{},
		{Boolean: &acpsdk.SetSessionConfigOptionBoolean{SessionId: created.SessionId, ConfigId: "mode", Value: true}},
		{Boolean: &acpsdk.SetSessionConfigOptionBoolean{}, ValueId: &acpsdk.SetSessionConfigOptionValueId{}},
		configRequest(created.SessionId, "bogus", "value"), configRequest(created.SessionId, "mode", "unsafe"), configRequest(created.SessionId, "mode", "AUTONOMOUS"), configRequest(created.SessionId, "model", "openai/arbitrary"), configRequest(created.SessionId, "thought_level", "nonsense"),
	} {
		_, err := a.SetSessionConfigOption(t.Context(), request)
		require.Error(t, err)
	}
	assert.Zero(t, creations.Load())
	assert.Equal(t, session.SafetyPolicy(""), s.sess.GetSafetyPolicy())
	_, finish, err := s.startTurn(t.Context())
	require.NoError(t, err)
	_, err = a.SetSessionConfigOption(t.Context(), configRequest(created.SessionId, "mode", "autonomous"))
	require.Error(t, err)
	finish()
	assert.Equal(t, session.SafetyPolicy(""), s.sess.GetSafetyPolicy())
	_, err = a.SetSessionMode(t.Context(), acpsdk.SetSessionModeRequest{SessionId: "missing", ModeId: "strict"})
	require.Error(t, err)
}

func TestSessionConfigurationCommandSwitchRefreshesSelection(t *testing.T) {
	t.Parallel()
	a, _ := newConfigurationAgent(t, session.NewInMemorySessionStore())
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	response, err := a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("/worker")}})
	require.NoError(t, err)
	assert.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
	configOptions := s.configuration(t.Context()).Options
	assert.Equal(t, "default", configValue(t, configOptions, "thought_level"))
	for _, option := range configOptions {
		if option.Select.Id == "model" {
			assert.Contains(t, option.Select.Name, "worker")
		}
	}
}

type blockedConfigStore struct {
	session.Store

	entered, release chan struct{}
}

func (s *blockedConfigStore) UpdateSession(ctx context.Context, value *session.Session) error {
	close(s.entered)
	<-s.release
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.UpdateSession(ctx, value)
}

func TestSessionConfigurationCloseJoinsPersistence(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a, _ := newConfigurationAgent(t, session.NewInMemorySessionStore())
		defer func() { require.NoError(t, a.Stop(t.Context())) }()
		created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
		require.NoError(t, err)
		store := &blockedConfigStore{Store: a.sessionStore, entered: make(chan struct{}), release: make(chan struct{})}
		a.sessionStore = store
		configured := make(chan error, 1)
		go func() {
			_, err := a.SetSessionMode(t.Context(), acpsdk.SetSessionModeRequest{SessionId: created.SessionId, ModeId: "autonomous"})
			configured <- err
		}()
		<-store.entered
		_, err = a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("must not run")}})
		require.Error(t, err)
		closed := closeSessionAsync(a, t.Context(), string(created.SessionId))
		synctest.Wait()
		assertPending(t, closed)
		close(store.release)
		require.ErrorIs(t, <-configured, context.Canceled)
		require.NoError(t, <-closed)
	})
}

func TestSessionConfigurationExplicitNoneAndDefaultRemainDistinct(t *testing.T) {
	t.Parallel()
	a, _ := newConfigurationAgent(t, session.NewInMemorySessionStore())
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	_, err = a.SetSessionConfigOption(t.Context(), configRequest(created.SessionId, "model", "model:alternative"))
	require.NoError(t, err)
	response, err := a.SetSessionConfigOption(t.Context(), configRequest(created.SessionId, "thought_level", "none"))
	require.NoError(t, err)
	assert.Equal(t, "none", configValue(t, response.ConfigOptions, "thought_level"))
	s := a.sessions[string(created.SessionId)]
	// Providers may normalize disabled thinking to nil; that is not the configured adaptive budget.
	root, err := s.team.Agent("root")
	require.NoError(t, err)
	cfg := root.EffectiveModels()[0].BaseConfig().ModelConfig
	assert.True(t, cfg.ThinkingBudget == nil || cfg.ThinkingBudget.IsDisabled())
	resumed, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: s.workingDir})
	require.NoError(t, err)
	assert.Equal(t, "none", configValue(t, resumed.ConfigOptions, "thought_level"))
	response, err = a.SetSessionConfigOption(t.Context(), configRequest(created.SessionId, "thought_level", "default"))
	require.NoError(t, err)
	assert.Equal(t, "default", configValue(t, response.ConfigOptions, "thought_level"))
	assert.True(t, root.EffectiveModels()[0].BaseConfig().ModelConfig.ThinkingBudget.IsAdaptive())
}

func TestSessionConfigurationPersistsMatchingRuntimeSelection(t *testing.T) {
	t.Parallel()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	a, creations := newConfigurationAgent(t, store)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	_, err = a.SetSessionConfigOption(t.Context(), configRequest(created.SessionId, "model", "model:default"))
	require.NoError(t, err)
	// model_picker and runtime callers change providers without persisting a session selection.
	require.NoError(t, s.rt.SetAgentModel(t.Context(), "root", "alternative"))
	before := creations.Load()
	_, err = a.SetSessionConfigOption(t.Context(), configRequest(created.SessionId, "model", "model:alternative"))
	require.NoError(t, err)
	assert.Equal(t, before, creations.Load(), "persistence-only selection should not rebuild the provider")
	_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	loaded, err := a.LoadSession(t.Context(), loadRequest(s))
	require.NoError(t, err)
	assert.Equal(t, "model:alternative", configValue(t, loaded.ConfigOptions, "model"))
}

type activeConfigRuntime struct {
	runtime.Runtime

	active bool
}

func (r *activeConfigRuntime) HasActiveWork() bool { return r.active }
func (r *activeConfigRuntime) AgentThinkingConfiguration(name string) ([]effort.Level, effort.Level) {
	return r.Runtime.(configurationRuntime).AgentThinkingConfiguration(name)
}

func TestSessionConfigurationBackgroundWorkBlocksMutationAndHidesVolatileSelectors(t *testing.T) {
	t.Parallel()
	a, creations := newConfigurationAgent(t, session.NewInMemorySessionStore())
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	s.rt = &activeConfigRuntime{Runtime: s.rt, active: true}
	for id, value := range map[string]string{"mode": "autonomous", "model": "model:alternative", "thought_level": "high"} {
		_, err := a.SetSessionConfigOption(t.Context(), configRequest(created.SessionId, id, value))
		require.ErrorContains(t, err, "active work")
	}
	assert.Zero(t, creations.Load())
	assert.Equal(t, session.SafetyPolicy(""), s.sess.GetSafetyPolicy())
	resumed, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: s.workingDir})
	require.NoError(t, err)
	require.Len(t, resumed.ConfigOptions, 1)
	assert.Equal(t, "mode", string(resumed.ConfigOptions[0].Select.Id))
	loaded, err := a.LoadSession(t.Context(), loadRequest(s))
	require.NoError(t, err)
	require.Len(t, loaded.ConfigOptions, 1)
}

func TestSessionConfigurationUnsupportedModelKinds(t *testing.T) {
	t.Parallel()
	a, _ := newConfigurationAgent(t, session.NewInMemorySessionStore())
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	s.configModels["alloy"] = latest.ModelConfig{Model: "default,alternative"}
	s.configModels["nested"] = latest.ModelConfig{Model: "alloy,default"}
	state := s.configuration(t.Context())
	for _, option := range state.Options {
		if option.Select.Id == "model" {
			var values []acpsdk.SessionConfigValueId
			for _, choice := range *option.Select.Options.Ungrouped {
				values = append(values, choice.Value)
			}
			assert.Contains(t, values, acpsdk.SessionConfigValueId("model:alloy"))
			assert.NotContains(t, values, acpsdk.SessionConfigValueId("model:nested"))
		}
	}
	response, err := a.SetSessionConfigOption(t.Context(), configRequest(created.SessionId, "model", "model:alloy"))
	require.NoError(t, err)
	assert.Equal(t, "model:alloy", configValue(t, response.ConfigOptions, "model"))
	require.Len(t, response.ConfigOptions, 2)
	_, err = a.SetSessionConfigOption(t.Context(), configRequest(created.SessionId, "model", "default"))
	require.NoError(t, err)
	root, err := s.team.Agent("root")
	require.NoError(t, err)
	agent.WithHarness(&latest.HarnessConfig{Type: "codex"})(root)
	state = s.configuration(t.Context())
	require.Len(t, state.Options, 1)
	assert.Equal(t, "mode", string(state.Options[0].Select.Id))
}

func TestSessionConfigurationPostCommitNotificationFailureDoesNotRollback(t *testing.T) {
	t.Parallel()
	a, _ := newConfigurationAgent(t, session.NewInMemorySessionStore())
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	captureReplay(t, a, &captureWriter{failOn: func(int) error { return io.ErrClosedPipe }})
	_, err = a.SetSessionConfigOption(t.Context(), configRequest(created.SessionId, "mode", "strict"))
	require.Error(t, err)
	s := a.sessions[string(created.SessionId)]
	assert.Equal(t, session.SafetyPolicyStrict, s.sess.GetSafetyPolicy())
	stored, err := a.sessionStore.GetSession(t.Context(), s.id)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyStrict, stored.GetSafetyPolicy())
}

func TestSessionConfigurationModelCancellationRestoresExactProviders(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a, _ := newConfigurationAgent(t, session.NewInMemorySessionStore())
		defer func() { require.NoError(t, a.Stop(t.Context())) }()
		created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
		require.NoError(t, err)
		s := a.sessions[string(created.SessionId)]
		root, err := s.team.Agent("root")
		require.NoError(t, err)
		_, err = a.SetSessionConfigOption(t.Context(), configRequest(created.SessionId, "thought_level", "high"))
		require.NoError(t, err)
		before := root.EffectiveModels()
		store := &blockedConfigStore{Store: a.sessionStore, entered: make(chan struct{}), release: make(chan struct{})}
		a.sessionStore = store
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := a.SetSessionConfigOption(ctx, configRequest(created.SessionId, "model", "model:alternative"))
			done <- err
		}()
		<-store.entered
		cancel()
		close(store.release)
		require.ErrorIs(t, <-done, context.Canceled)
		assert.Equal(t, before, root.EffectiveModels())
		assert.Empty(t, s.sess.AgentModelOverrides)
		assert.Equal(t, "high", configValue(t, s.configuration(t.Context()).Options, "thought_level"))
	})
}

func TestSessionConfigurationSelectorsReturnAfterBusyReconnect(t *testing.T) {
	t.Parallel()
	for _, load := range []bool{false, true} {
		a, _ := newConfigurationAgent(t, session.NewInMemorySessionStore())
		t.Cleanup(func() { require.NoError(t, a.Stop(t.Context())) })
		out := &captureWriter{}
		captureReplay(t, a, out)
		created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
		require.NoError(t, err)
		s := a.sessions[string(created.SessionId)]
		a.refreshConfiguration(t.Context(), s)
		wrapper := &activeConfigRuntime{Runtime: s.rt, active: true}
		s.rt = wrapper
		if load {
			_, err = a.LoadSession(t.Context(), loadRequest(s))
		} else {
			_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: s.workingDir})
		}
		require.NoError(t, err)
		before := len(out.lines())
		wrapper.active = false
		a.refreshConfiguration(t.Context(), s)
		require.Greater(t, len(out.lines()), before, "reconnect responses invalidate the prior notification baseline")
		updates := replayUpdates(t, out, s.id)
		var configOptions []acpsdk.SessionConfigOption
		for _, update := range updates {
			if update.ConfigOptionUpdate != nil {
				configOptions = update.ConfigOptionUpdate.ConfigOptions
			}
		}
		assert.Equal(t, "default", configValue(t, configOptions, "model"))
		assert.Equal(t, "default", configValue(t, configOptions, "thought_level"))
	}
}

func TestSessionConfigurationStalePersistedModelFailsColdLoad(t *testing.T) {
	t.Parallel()
	a, _ := newConfigurationAgent(t, session.NewInMemorySessionStore())
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	stored, err := a.sessionStore.GetSession(t.Context(), s.id)
	require.NoError(t, err)
	stored.SetAgentModelOverride("root", "missing-configured-model")
	require.NoError(t, a.sessionStore.UpdateSession(t.Context(), stored))
	_, err = a.LoadSession(t.Context(), loadRequest(s))
	require.ErrorContains(t, err, "restoring model override")
	assert.Empty(t, a.sessions)
}

func TestSessionConfigurationWireContract(t *testing.T) {
	t.Parallel()
	a, _ := newConfigurationAgent(t, session.NewInMemorySessionStore())
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	input, send := io.Pipe()
	receive, output := io.Pipe()
	conn := a.NewConnection(output, input)
	t.Cleanup(func() { _ = send.Close(); _ = receive.Close(); <-conn.Done() })
	encoder, decoder := json.NewEncoder(send), json.NewDecoder(receive)
	for i, tc := range []struct {
		method    string
		params    map[string]any
		wantError bool
	}{
		{method: "session/set_config_option", params: map[string]any{"configId": "model", "value": "model:default"}},
		{method: "session/set_config_option", params: map[string]any{"configId": "thought_level", "value": "high"}},
		{method: "session/set_mode", params: map[string]any{"modeId": "strict"}},
		{method: "session/set_config_option", params: map[string]any{"configId": "mode", "value": "autonomous"}},
		{method: "session/set_config_option", params: map[string]any{"configId": "mode", "type": "boolean", "value": true}, wantError: true},
		{method: "session/set_config_option", params: map[string]any{"configId": "mode", "value": "unsafe"}, wantError: true},
		{method: "session/set_mode", params: map[string]any{"modeId": "bogus"}, wantError: true},
	} {
		tc.params["sessionId"] = created.SessionId
		require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": i, "method": tc.method, "params": tc.params}))
		var notified []acpsdk.SessionConfigOption
		var mode acpsdk.SessionModeId
		for {
			var message struct {
				ID     *int                       `json:"id"`
				Params acpsdk.SessionNotification `json:"params"`
				Result json.RawMessage            `json:"result"`
				Error  *acpsdk.RequestError       `json:"error"`
			}
			require.NoError(t, decoder.Decode(&message))
			if message.ID == nil {
				if update := message.Params.Update.ConfigOptionUpdate; update != nil {
					notified = update.ConfigOptions
				}
				if update := message.Params.Update.CurrentModeUpdate; update != nil {
					mode = update.CurrentModeId
				}
				continue
			}
			assert.Equal(t, i, *message.ID)
			if tc.wantError {
				require.NotNil(t, message.Error)
				assert.Equal(t, -32602, message.Error.Code)
				assert.Nil(t, notified)
				break
			}
			require.Nil(t, message.Error)
			require.NotEmpty(t, notified)
			for _, option := range notified {
				require.NotNil(t, option.Select)
				assert.Equal(t, "select", option.Select.Type)
				assert.True(t, slices.ContainsFunc(*option.Select.Options.Ungrouped, func(value acpsdk.SessionConfigSelectOption) bool { return value.Value == option.Select.CurrentValue }))
			}
			if tc.method == "session/set_config_option" {
				var response acpsdk.SetSessionConfigOptionResponse
				require.NoError(t, json.Unmarshal(message.Result, &response))
				assert.Equal(t, notified, response.ConfigOptions)
			} else {
				assert.JSONEq(t, `{}`, string(message.Result))
				assert.Equal(t, acpsdk.SessionModeId("strict"), mode)
			}
			break
		}
	}
}
