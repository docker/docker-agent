package config

import (
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

type RuntimeConfig struct {
	Config

	EnvProviderForTests environment.Provider
	envProviderCached   environment.Provider
	envProviderOnce     sync.Once

	ModelsDevStoreOverride *modelsdev.Store
	modelsDevStore         *modelsdev.Store
	modelsDevStoreErr      error
	modelsDevStoreOnce     sync.Once
}

type Config struct {
	EnvFiles       []string
	ModelsGateway  string
	DefaultModel   *latest.ModelConfig
	GlobalCodeMode bool
	WorkingDir     string
	Models         map[string]latest.ModelConfig
	Providers      map[string]latest.ProviderConfig

	// Hook overrides from CLI flags
	HookPreToolUse   []string
	HookPostToolUse  []string
	HookSessionStart []string
	HookSessionEnd   []string
	HookOnUserInput  []string
	HookStop         []string

	MCPToolName  string
	MCPKeepAlive time.Duration

	// MCPOAuthRedirectURI is an opaque public HTTPS URL the runtime advertises
	// as the OAuth `redirect_uri` when running an MCP server OAuth flow in
	// unmanaged mode (see WithManagedOAuth(false)). When set, docker-agent
	// generates state + PKCE + DCR in-process and emits an elicitation
	// carrying the `authorize_url` + `state`; the client is then a thin
	// relay that opens the browser, receives the callback (typically via a
	// host-controlled bouncer + deeplink), and returns {code, state} via
	// ResumeElicitation. docker-agent then exchanges the code for the
	// token using this same URI as redirect_uri (RFC 6749 §4.1.3 requires
	// the value to match the one sent at the /authorize step).
	//
	// When empty, the unmanaged flow keeps its original contract: the
	// client is expected to drive the OAuth dance end-to-end and return
	// {access_token, refresh_token, …}. This preserves backward compat
	// with existing CLI-mirror clients.
	//
	// The URI itself is opaque to docker-agent — what it points at and how
	// the browser eventually lands back in the host application is the
	// caller's concern.
	MCPOAuthRedirectURI string
}

func (runConfig *RuntimeConfig) Clone() *RuntimeConfig {
	store, storeErr := runConfig.ModelsDevStore()
	env := runConfig.EnvProvider()
	clone := &RuntimeConfig{
		Config:                 runConfig.Config,
		EnvProviderForTests:    runConfig.EnvProviderForTests,
		envProviderCached:      env,
		ModelsDevStoreOverride: runConfig.ModelsDevStoreOverride,
		modelsDevStore:         store,
		modelsDevStoreErr:      storeErr,
	}
	clone.envProviderOnce.Do(func() {})    // mark as resolved
	clone.modelsDevStoreOnce.Do(func() {}) // mark as resolved
	clone.EnvFiles = slices.Clone(runConfig.EnvFiles)
	clone.Models = maps.Clone(runConfig.Models)
	clone.Providers = maps.Clone(runConfig.Providers)
	clone.DefaultModel = runConfig.DefaultModel.Clone()
	clone.HookPreToolUse = slices.Clone(runConfig.HookPreToolUse)
	clone.HookPostToolUse = slices.Clone(runConfig.HookPostToolUse)
	clone.HookSessionStart = slices.Clone(runConfig.HookSessionStart)
	clone.HookSessionEnd = slices.Clone(runConfig.HookSessionEnd)
	clone.HookOnUserInput = slices.Clone(runConfig.HookOnUserInput)
	clone.HookStop = slices.Clone(runConfig.HookStop)
	return clone
}

// ModelsDevStore returns the lazily-initialized models.dev store.
// The store is created on first access and shared across clones.
// If ModelsDevStoreOverride is set, it is returned directly.
func (runConfig *RuntimeConfig) ModelsDevStore() (*modelsdev.Store, error) {
	if runConfig.ModelsDevStoreOverride != nil {
		return runConfig.ModelsDevStoreOverride, nil
	}
	runConfig.modelsDevStoreOnce.Do(func() {
		runConfig.modelsDevStore, runConfig.modelsDevStoreErr = modelsdev.NewStore(
			modelsdev.WithKnownProvider(provider.IsKnownProvider),
		)
	})
	return runConfig.modelsDevStore, runConfig.modelsDevStoreErr
}

func (runConfig *RuntimeConfig) EnvProvider() environment.Provider {
	if runConfig.EnvProviderForTests != nil {
		return runConfig.EnvProviderForTests
	}

	runConfig.envProviderOnce.Do(func() {
		runConfig.envProviderCached = runConfig.computedEnvProvider()
	})
	return runConfig.envProviderCached
}

func (runConfig *RuntimeConfig) computedEnvProvider() environment.Provider {
	defaultEnv := environment.NewDefaultProvider()

	// Make env file paths absolute relative to the working directory.
	var err error
	runConfig.EnvFiles, err = environment.AbsolutePaths(runConfig.WorkingDir, runConfig.EnvFiles)
	if err != nil {
		slog.Error("Failed to make env file paths absolute", "error", err)
		return defaultEnv
	}

	envFilesProviders, err := environment.NewEnvFilesProvider(runConfig.EnvFiles)
	if err != nil {
		slog.Error("Failed to read env files", "error", err)
		return defaultEnv
	}

	// Update the env provider to include env files
	return environment.NewMultiProvider(envFilesProviders, defaultEnv)
}
