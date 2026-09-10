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
	envFilesErr         error

	ModelsDevStoreOverride *modelsdev.Store
	modelsDevStore         *modelsdev.Store
	modelsDevStoreErr      error
	modelsDevStoreOnce     sync.Once

	// ProviderRegistry instantiates model providers for toolsets that build
	// providers at load time (e.g. RAG embeddings/reranking). It is populated
	// by the team loader with the same registry used for agent models. When
	// nil, ProviderRegistryOrDefault falls back to provider.DefaultRegistry.
	ProviderRegistry *provider.Registry
}

type Config struct {
	EnvFiles       []string
	ModelsGateway  string
	DefaultModel   *latest.ModelConfig
	GlobalCodeMode bool
	WorkingDir     string
	Models         map[string]latest.ModelConfig
	Providers      map[string]latest.ProviderConfig

	// EncryptedConfig is an opaque, encrypted representation of the full agent
	// YAML (as produced by `docker agent share push --key --encrypt`). When set
	// and the configured models gateway is a trusted Docker gateway, it is
	// forwarded to the gateway on every request via the
	// `X-Cagent-Encrypted-Config` header. It is ignored for non-Docker
	// gateways and when no gateway is configured.
	EncryptedConfig string

	// Flavors are the config flavor patches to enable when loading agent
	// configs, applied in order. Names a config does not define are ignored.
	Flavors []string

	// Hook overrides from user config and CLI flags
	GlobalHooks      *latest.HooksConfig
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
		envFilesErr:            runConfig.envFilesErr,
		ModelsDevStoreOverride: runConfig.ModelsDevStoreOverride,
		modelsDevStore:         store,
		modelsDevStoreErr:      storeErr,
		ProviderRegistry:       runConfig.ProviderRegistry,
	}
	clone.envProviderOnce.Do(func() {})    // mark as resolved
	clone.modelsDevStoreOnce.Do(func() {}) // mark as resolved
	clone.EnvFiles = slices.Clone(runConfig.EnvFiles)
	clone.Flavors = slices.Clone(runConfig.Flavors)
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

// ProviderRegistryOrDefault returns the configured provider registry, or the
// package default registry when none was set (including when the receiver is
// nil). The default registry only contains providers the core package can
// expose without optional SDK dependencies, so callers that need the full
// provider set must ensure the team loader populated ProviderRegistry.
func (runConfig *RuntimeConfig) ProviderRegistryOrDefault() *provider.Registry {
	if runConfig != nil && runConfig.ProviderRegistry != nil {
		return runConfig.ProviderRegistry
	}
	return provider.DefaultRegistry()
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

// EnvFilesError reports a failure to resolve, read, or parse the configured
// env files (--env-from-file). The provider chain silently falls back to the
// default sources in that case, so callers that must not run without the
// file's variables (e.g. the CLI) should surface this error and abort.
func (runConfig *RuntimeConfig) EnvFilesError() error {
	runConfig.EnvProvider() // materialize the provider chain
	return runConfig.envFilesErr
}

func (runConfig *RuntimeConfig) computedEnvProvider() environment.Provider {
	defaultEnv := environment.NewDefaultProvider()

	// Make env file paths absolute relative to the working directory.
	var err error
	runConfig.EnvFiles, err = environment.AbsolutePaths(runConfig.WorkingDir, runConfig.EnvFiles)
	if err != nil {
		slog.Error("Failed to make env file paths absolute", "error", err)
		runConfig.envFilesErr = err
		return defaultEnv
	}

	envFilesProviders, err := environment.NewEnvFilesProvider(runConfig.EnvFiles)
	if err != nil {
		slog.Error("Failed to read env files", "error", err)
		runConfig.envFilesErr = err
		return defaultEnv
	}

	// Update the env provider to include env files
	return environment.NewMultiProvider(envFilesProviders, defaultEnv)
}
