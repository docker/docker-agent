package config

import (
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
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
		runConfig.modelsDevStore, runConfig.modelsDevStoreErr = modelsdev.NewStore()
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
