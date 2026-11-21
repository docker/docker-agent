//go:build js && wasm && !docker_agent_wasm_small

package main

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"sync"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/legacy"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/modelsdev/embedded"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/openapi"
	"github.com/docker/docker-agent/pkg/tools/codemode"
)

// The default profile: every browser provider and toolset, all YAML
// versions, the embedded models.dev catalog, JavaScript expansion and code
// mode. small.go is the size-optimised alternative.

// browserFeatures are the optional loader features the browser enables;
// strict loading rejects configs that need any other.
var browserFeatures = []config.Feature{config.FeatureHooks, config.FeatureCodeMode, config.FeatureToon, config.FeatureDeferredTools}

// browserBuiltinHooks are the builtin hooks that neither run a process nor
// touch the filesystem or git; add_environment_info has a browser build that
// reports no shell or git and reads nothing from the process env.
var browserBuiltinHooks = []string{
	builtins.AddContext,
	builtins.AddDate,
	builtins.AddEnvironmentInfo,
	builtins.LimitLargeToolResults,
	builtins.MaxIterations,
	builtins.RedactSecrets,
}

// configVersions accepts every historical YAML version.
var configVersions config.VersionRegistrar = legacy.Register

// modelsStore is the models.dev catalog baked into the binary: the browser
// has no cache directory to refresh a live copy into.
var modelsStore = sync.OnceValue(func() *modelsdev.Store {
	return modelsdev.NewDatabaseStore(embedded.Snapshot())
})

// registerProviders adds Anthropic and Google, plus the cloud providers of
// cloud.go when the binary is built with -tags docker_agent_wasm_cloud;
// googleFactory comes from cloud.go or cloud_off.go accordingly.
func registerProviders(factories map[string]provider.Factory) {
	factories["anthropic"] = func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
		return anthropic.NewClient(ctx, cfg, env, opts...)
	}
	factories["google"] = googleFactory
	maps.Copy(factories, cloudFactories())
}

// profileLoadOpts enables code mode, which runs on goja.
var profileLoadOpts = []teamloader.Opt{teamloader.WithCodeMode(codemode.Wrap)}

func registerOpenAPI(creators map[string]teamloader.ToolsetCreator) {
	creators["openapi"] = openapiCreator
}

func openapiCreator(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, configName string) (tools.ToolSet, error) {
	if err := checkOpenAPI(ctx, toolset, runConfig.EnvProvider()); err != nil {
		return nil, err
	}
	return openapi.Creator(ctx, toolset, parentDir, runConfig, configName)
}

// checkOpenAPI accepts only specs fetched over HTTP(S). The URL is expanded
// with the session env first, like the toolset itself does.
func checkOpenAPI(ctx context.Context, ts latest.Toolset, env environment.Provider) error {
	spec := js.NewJsExpander(env).Expand(ctx, ts.URL, nil)
	u, err := url.Parse(spec)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("openapi url %q: only http(s) specs can be fetched in the browser", spec)
	}
	return nil
}
