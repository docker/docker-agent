package tui_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/dnaeon/go-vcr.v4/pkg/recorder"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/fake"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
	"github.com/docker/docker-agent/pkg/tui"
	"github.com/docker/docker-agent/pkg/tui/tuitest"
)

// newTUI builds the real top-level TUI model for agentFile, wired to a
// replaying VCR proxy so the agent's responses are deterministic and offline.
// It returns a started tuitest.Driver sized to the given terminal dimensions.
//
// State directories (data/config) are redirected to a temp dir so the test
// never touches the developer's ~/.cagent, and the SQLite session store lives
// under t.TempDir() too.
//
// agentFile is a deliberate seam: new scenarios point the harness at other
// agent configs without touching the helper.
//
//nolint:unparam // agentFile is intentionally parameterized for future scenarios.
func newTUI(t *testing.T, agentFile string, width, height int, tuiOpts ...tui.Option) *tuitest.Driver {
	t.Helper()
	return newTUIWithProxyOptions(t, agentFile, width, height, nil, tuiOpts...)
}

// newTUIWithProxyOptions is newTUI with control over the replay proxy, e.g.
// simulated SSE streaming so a scenario can interact with the TUI while the
// agent is still mid-stream (steering, queueing).
func newTUIWithProxyOptions(t *testing.T, agentFile string, width, height int, proxyOpts *fake.ProxyOptions, tuiOpts ...tui.Option) *tuitest.Driver {
	t.Helper()

	isolateState(t)

	runConfig := startReplayProxy(t, proxyOpts)

	ctx := t.Context()
	agentSource, err := sources.Resolve(agentFile, runConfig.EnvProvider())
	require.NoError(t, err)

	loadResult, err := teamloader.LoadWithConfig(ctx, agentSource, runConfig, loaderdefaults.Opts()...)
	require.NoError(t, err)

	team := loadResult.Team
	agent, err := team.AgentOrDefault("")
	require.NoError(t, err)

	store, err := sqlitestore.New(ctx, filepath.Join(t.TempDir(), "session.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	rt, err := runtime.New(ctx, team,
		runtime.WithSessionStore(store),
		runtime.WithCurrentAgent(agent.Name()),
		runtime.WithModelSwitcherConfig(&runtime.ModelSwitcherConfig{
			Models:             loadResult.Models,
			Providers:          loadResult.Providers,
			ModelsGateway:      runConfig.ModelsGateway,
			EnvProvider:        runConfig.EnvProvider(),
			ProviderRegistry:   loadResult.ProviderRegistry,
			AgentDefaultModels: loadResult.AgentDefaultModels,
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })

	var appOpts []app.Opt
	if gen := rt.TitleGenerator(ctx); gen != nil {
		appOpts = append(appOpts, app.WithTitleGenerator(gen))
	}
	application := app.New(ctx, rt, session.New(), appOpts...)

	wd, _ := os.Getwd()
	model := tui.New(ctx, nil /* no spawner: single tab */, application, wd, func() {}, tuiOpts...)

	return tuitest.New(t, model, width, height)
}

// isolateState redirects docker-agent's data and config directories to a temp
// dir for the duration of the test so the TUI's persistent state (tab store,
// user settings) never touches the real home directory.
func isolateState(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	paths.SetDataDir(filepath.Join(dir, "data"))
	paths.SetConfigDir(filepath.Join(dir, "config"))
	t.Cleanup(func() {
		paths.SetDataDir("")
		paths.SetConfigDir("")
	})
}

// startReplayProxy starts a VCR proxy in replay-only mode against the cassette
// named after the current test, and returns a RuntimeConfig pointed at it.
// Recordings live in testdata/cassettes/<TestName>.yaml. options may be nil;
// pass fake.ProxyOptions with SimulateStream to replay responses chunk by
// chunk with delays.
func startReplayProxy(t *testing.T, options *fake.ProxyOptions) *config.RuntimeConfig {
	t.Helper()

	cassettePath := filepath.Join("testdata", "cassettes", t.Name())

	matcher := fake.DefaultMatcher(func(err error) { require.NoError(t, err) })

	proxyURL, cleanup, err := fake.StartProxyWithOptions(
		t.Context(),
		cassettePath,
		recorder.ModeReplayOnly,
		matcher,
		func(string, *http.Request) {}, // no API keys needed for replay
		options,
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cleanup()) })

	return &config.RuntimeConfig{
		Config: config.Config{ModelsGateway: proxyURL},
		EnvProviderForTests: &mapEnvProvider{
			environment.DockerDesktopTokenEnv: "DUMMY",
		},
	}
}

// mapEnvProvider is a static environment.Provider for tests.
type mapEnvProvider map[string]string

func (p *mapEnvProvider) Get(_ context.Context, name string) (string, bool) {
	v, ok := (*p)[name]
	return v, ok
}
