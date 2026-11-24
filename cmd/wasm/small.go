//go:build js && wasm && docker_agent_wasm_small

package main

import (
	"context"
	"sync"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/teamloader"
)

// The small profile keeps the shared runtime, OpenAI-compatible providers and
// Anthropic. It leaves out Google, OpenAPI, historical YAML versions, the
// embedded models.dev catalog, and engines whose reflective method lookups stop
// the linker from pruning unused methods (goja, text/template).

// browserFeatures leaves code mode out: it runs on goja.
var browserFeatures = []config.Feature{config.FeatureHooks, config.FeatureToon, config.FeatureDeferredTools}

// browserBuiltinHooks leaves add_context out: it renders Go templates.
var browserBuiltinHooks = []string{
	builtins.AddDate,
	builtins.AddEnvironmentInfo,
	builtins.LimitLargeToolResults,
	builtins.MaxIterations,
	builtins.RedactSecrets,
}

// configVersions is nil: only the current YAML schema is accepted.
var configVersions config.VersionRegistrar

// modelsStore is empty: no catalog is baked in, and an explicit store keeps
// the runtime from looking for one on disk or on the network. Models then
// run with unknown context limits, costs and capabilities.
var modelsStore = sync.OnceValue(func() *modelsdev.Store {
	return modelsdev.NewDatabaseStore(&modelsdev.Database{})
})

func registerProviders(map[string]provider.Factory) {}

var profileLoadOpts []teamloader.Opt

func registerOpenAPI(map[string]teamloader.ToolsetCreator) {}

func checkOpenAPI(context.Context, latest.Toolset, environment.Provider) error {
	// Strict loading rejects the unregistered toolset before model construction.
	return nil
}
